package operatorwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/fsnotify/fsnotify"
)

const (
	DefaultInboxPath     = "~/.workbuddy/operator/inbox"
	DefaultConfigDir     = ".github/workbuddy"
	DefaultDBPath        = ".workbuddy/workbuddy.db"
	DefaultTimeout       = 10 * time.Minute
	DefaultPauseInterval = 30 * time.Second
	DefaultEventRepo     = "operator"
)

type Logger interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

type ConfigLoader func(configDir string) (*config.FullConfig, []config.Warning, error)

type Runner interface {
	Run(ctx context.Context, claudePath, incidentPath string) (int, error)
}

type Options struct {
	InboxDir      string
	ClaudePath    string
	ConfigDir     string
	Timeout       time.Duration
	PauseInterval time.Duration
	DryRun        bool
	Stdout        io.Writer
	Stderr        io.Writer
	Logger        Logger
	LoadConfig    ConfigLoader
	Runner        Runner
}

type Service struct {
	opts       Options
	logger     *log.Logger
	dryRunSeen map[string]time.Time
}

type invocationPayload struct {
	IncidentID string `json:"incident_id"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
}

type incidentMetadata struct {
	IncidentID string
	Repo       string
	IssueNum   int
}

func Run(ctx context.Context, opts Options) error {
	svc, err := New(opts)
	if err != nil {
		return err
	}
	return svc.Run(ctx)
}

func New(opts Options) (*Service, error) {
	if strings.TrimSpace(opts.InboxDir) == "" {
		opts.InboxDir = DefaultInboxPath
	}
	if strings.TrimSpace(opts.ConfigDir) == "" {
		opts.ConfigDir = DefaultConfigDir
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.PauseInterval <= 0 {
		opts.PauseInterval = DefaultPauseInterval
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.LoadConfig == nil {
		opts.LoadConfig = config.LoadConfig
	}
	if opts.Runner == nil {
		opts.Runner = commandRunner{}
	}

	inboxDir, err := expandPath(opts.InboxDir)
	if err != nil {
		return nil, fmt.Errorf("operator-watch: expand inbox: %w", err)
	}
	opts.InboxDir = inboxDir
	opts.ConfigDir = strings.TrimSpace(opts.ConfigDir)
	opts.ClaudePath = strings.TrimSpace(opts.ClaudePath)

	return &Service{
		opts:       opts,
		logger:     log.New(opts.Stderr, "[operator-watch] ", log.LstdFlags),
		dryRunSeen: make(map[string]time.Time),
	}, nil
}

func (s *Service) Run(ctx context.Context) error {
	if err := s.ensureLayout(); err != nil {
		return err
	}
	if err := s.recoverProcessingFiles(); err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("operator-watch: create watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(s.opts.InboxDir); err != nil {
		return fmt.Errorf("operator-watch: watch inbox: %w", err)
	}

	ticker := time.NewTicker(s.opts.PauseInterval)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		allowed, err := s.dispatchAllowed()
		if err != nil {
			s.logger.Printf("config check failed: %v", err)
		}
		if !allowed || err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				continue
			case watchErr := <-watcher.Errors:
				if watchErr != nil {
					s.logger.Printf("watch error while paused: %v", watchErr)
				}
			case <-watcher.Events:
			}
			continue
		}

		processedAny, err := s.processPending(ctx)
		if err != nil {
			return err
		}
		if processedAny {
			continue
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case watchErr := <-watcher.Errors:
			if watchErr != nil {
				s.logger.Printf("watch error: %v", watchErr)
			}
		case evt := <-watcher.Events:
			if evt.Name != "" && evt.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write) != 0 {
				s.logger.Printf("detected inbox activity: %s", evt.Name)
			}
		}
	}
}

func (s *Service) ensureLayout() error {
	for _, dir := range []string{s.opts.InboxDir, s.processedDir(), s.failedDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("operator-watch: mkdir %s: %w", dir, err)
		}
	}
	return nil
}

func (s *Service) processPending(ctx context.Context) (bool, error) {
	paths, err := s.pendingIncidentPaths()
	if err != nil {
		return false, err
	}
	if len(paths) > 50 {
		s.logger.Printf("pending inbox backlog=%d exceeds backpressure threshold 50", len(paths))
	}
	if len(paths) == 0 {
		return false, nil
	}

	for _, path := range paths {
		allowed, err := s.dispatchAllowed()
		if err != nil {
			s.logger.Printf("config re-check failed before dispatch: %v", err)
			return false, nil
		}
		if !allowed {
			return false, nil
		}

		if s.opts.DryRun {
			info, statErr := os.Stat(path)
			if statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					return false, nil
				}
				return false, fmt.Errorf("operator-watch: stat dry-run incident: %w", statErr)
			}
			if lastSeen, ok := s.dryRunSeen[path]; ok && lastSeen.Equal(info.ModTime()) {
				continue
			}
			s.dryRunSeen[path] = info.ModTime()
			fmt.Fprintf(s.opts.Stdout, "dry-run: would invoke claude for %s\n", path)
			return true, nil
		}

		if err := s.processIncident(ctx, path); err != nil {
			return true, err
		}
		return true, nil
	}
	return false, nil
}

func (s *Service) processIncident(ctx context.Context, originalPath string) error {
	processingPath := processingPath(originalPath)
	if err := os.Rename(originalPath, processingPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("operator-watch: claim %s: %w", originalPath, err)
	}

	meta := readIncidentMetadata(processingPath)
	started := time.Now()

	runCtx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	exitCode, runErr := s.opts.Runner.Run(runCtx, s.opts.ClaudePath, processingPath)
	duration := time.Since(started)

	targetPath := filepath.Join(s.processedDir(), filepath.Base(originalPath))
	if runErr == nil && exitCode == 0 {
		if err := os.Rename(processingPath, targetPath); err != nil {
			return fmt.Errorf("operator-watch: move processed: %w", err)
		}
		s.recordInvocation(meta, exitCode, duration)
		return nil
	}

	if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		targetPath = filepath.Join(s.failedDir(), fmt.Sprintf("timeout-%s", incidentFilename(meta.IncidentID)))
		exitCode = -1
	} else if exitCode != 0 {
		targetPath = filepath.Join(s.failedDir(), fmt.Sprintf("exit-%d-%s", exitCode, incidentFilename(meta.IncidentID)))
	} else {
		exitCode = -1
		targetPath = filepath.Join(s.failedDir(), fmt.Sprintf("error-%s", incidentFilename(meta.IncidentID)))
	}
	if err := os.Rename(processingPath, targetPath); err != nil {
		return fmt.Errorf("operator-watch: move failed: %w", err)
	}
	s.recordInvocation(meta, exitCode, duration)
	if runErr != nil && !errors.Is(runErr, context.DeadlineExceeded) {
		s.logger.Printf("incident %s failed: %v", meta.IncidentID, runErr)
	}
	return nil
}

func (s *Service) recoverProcessingFiles() error {
	matches, err := filepath.Glob(filepath.Join(s.opts.InboxDir, "*.processing"))
	if err != nil {
		return fmt.Errorf("operator-watch: glob processing files: %w", err)
	}
	for _, path := range matches {
		meta := readIncidentMetadata(path)
		target := filepath.Join(s.failedDir(), fmt.Sprintf("crash-%s", incidentFilename(meta.IncidentID)))
		if err := os.Rename(path, target); err != nil {
			return fmt.Errorf("operator-watch: recover %s: %w", path, err)
		}
	}
	return nil
}

func (s *Service) dispatchAllowed() (bool, error) {
	pausedPath := filepath.Join(filepath.Dir(s.opts.InboxDir), "paused")
	if _, err := os.Stat(pausedPath); err == nil {
		return false, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat paused file: %w", err)
	}

	cfg, _, err := s.opts.LoadConfig(s.opts.ConfigDir)
	if err != nil {
		return false, fmt.Errorf("load config: %w", err)
	}
	return cfg.Operator.Enabled, nil
}

func (s *Service) recordInvocation(meta incidentMetadata, exitCode int, duration time.Duration) {
	if s.opts.Logger == nil {
		return
	}
	repo := strings.TrimSpace(meta.Repo)
	if repo == "" {
		repo = DefaultEventRepo
	}
	s.opts.Logger.Log(eventlog.TypeOperatorInvoked, repo, meta.IssueNum, invocationPayload{
		IncidentID: meta.IncidentID,
		ExitCode:   exitCode,
		DurationMS: duration.Milliseconds(),
	})
}

func (s *Service) pendingIncidentPaths() ([]string, error) {
	entries, err := os.ReadDir(s.opts.InboxDir)
	if err != nil {
		return nil, fmt.Errorf("operator-watch: read inbox: %w", err)
	}

	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, "."):
			continue
		case strings.HasSuffix(name, ".processing"):
			continue
		case !strings.HasSuffix(strings.ToLower(name), ".json"):
			continue
		default:
			paths = append(paths, filepath.Join(s.opts.InboxDir, name))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func (s *Service) processedDir() string {
	return filepath.Join(filepath.Dir(s.opts.InboxDir), "processed")
}

func (s *Service) failedDir() string {
	return filepath.Join(filepath.Dir(s.opts.InboxDir), "failed")
}

func readIncidentMetadata(path string) incidentMetadata {
	meta := incidentMetadata{
		IncidentID: incidentIDFromPath(path),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return meta
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return meta
	}
	if v, ok := raw["incident_id"]; ok {
		var incidentID string
		if json.Unmarshal(v, &incidentID) == nil && strings.TrimSpace(incidentID) != "" {
			meta.IncidentID = incidentID
		}
	}
	if v, ok := raw["repo"]; ok {
		_ = json.Unmarshal(v, &meta.Repo)
	}
	if v, ok := raw["issue_num"]; ok {
		_ = json.Unmarshal(v, &meta.IssueNum)
	}
	return meta
}

func expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func processingPath(originalPath string) string {
	return originalPath + ".processing"
}

func incidentIDFromPath(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".processing")
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func incidentFilename(incidentID string) string {
	incidentID = strings.TrimSpace(incidentID)
	if incidentID == "" {
		incidentID = "unknown"
	}
	return incidentID + ".json"
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, claudePath, incidentPath string) (int, error) {
	bin := strings.TrimSpace(claudePath)
	if bin == "" {
		bin = "claude"
	}

	prompt := fmt.Sprintf("/workbuddy:handle-incident %s", incidentPath)
	cmd := exec.CommandContext(ctx, bin, "-p", "--plugin", "workbuddy", prompt)
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		fmt.Fprint(os.Stderr, string(output))
	}
	if err == nil {
		if cmd.ProcessState == nil {
			return 0, nil
		}
		return cmd.ProcessState.ExitCode(), nil
	}
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), err
	}
	return -1, err
}
