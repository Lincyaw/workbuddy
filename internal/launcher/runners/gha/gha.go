package gha

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type CommandRunner interface {
	Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
}

type GHCLI struct{}

func (GHCLI) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

type Client struct {
	runner CommandRunner
	now    func() time.Time
	sleep  func(time.Duration)
}

func NewClient() *Client {
	return &Client{
		runner: GHCLI{},
		now:    time.Now,
		sleep:  time.Sleep,
	}
}

func NewClientWithRunner(runner CommandRunner) *Client {
	c := NewClient()
	c.runner = runner
	return c
}

type Config struct {
	Repo         string
	IssueNumber  int
	AgentName    string
	SessionID    string
	Workflow     string
	Ref          string
	PollInterval time.Duration
	ArtifactDir  string
}

type Run struct {
	ID         int64
	HTMLURL    string
	Status     string
	Conclusion string
	HeadBranch string
	CreatedAt  time.Time
}

type dispatchResult struct {
	RunID   int64
	RunURL  string
	APIURL  string
	LogsURL string
}

type Outcome struct {
	Run                  Run
	LogPath              string
	Logs                 string
	ArtifactDir          string
	Files                []string
	CanonicalSessionPath string
	ResultPath           string
}

func (c *Client) RunWorkflow(ctx context.Context, cfg Config) (*Outcome, error) {
	if strings.TrimSpace(cfg.Repo) == "" {
		return nil, errors.New("gha: repo is required")
	}
	if cfg.IssueNumber <= 0 {
		return nil, errors.New("gha: issue number must be > 0")
	}
	if strings.TrimSpace(cfg.AgentName) == "" {
		return nil, errors.New("gha: agent name is required")
	}
	if strings.TrimSpace(cfg.SessionID) == "" {
		return nil, errors.New("gha: session ID is required")
	}
	if strings.TrimSpace(cfg.Workflow) == "" {
		return nil, errors.New("gha: workflow is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.ArtifactDir == "" {
		cfg.ArtifactDir = "."
	}
	if err := os.MkdirAll(cfg.ArtifactDir, 0o755); err != nil {
		return nil, fmt.Errorf("gha: create artifact dir: %w", err)
	}

	dispatchedAt := c.now().UTC().Add(-2 * time.Second)
	dispatchRun, err := c.dispatch(ctx, cfg)
	if err != nil {
		return nil, err
	}
	run, err := c.waitForRun(ctx, cfg, dispatchedAt, dispatchRun)
	if err != nil {
		return nil, err
	}

	logPath, logs, err := c.downloadLogs(ctx, cfg, run.ID)
	if err != nil {
		return nil, err
	}
	files, resultPath, sessionPath, err := c.downloadArtifacts(ctx, cfg, run.ID)
	if err != nil {
		return nil, err
	}

	return &Outcome{
		Run:                  run,
		LogPath:              logPath,
		Logs:                 logs,
		ArtifactDir:          cfg.ArtifactDir,
		Files:                files,
		CanonicalSessionPath: sessionPath,
		ResultPath:           resultPath,
	}, nil
}

func (c *Client) dispatch(ctx context.Context, cfg Config) (dispatchResult, error) {
	args := []string{
		"api", "-X", "POST",
		fmt.Sprintf("repos/%s/actions/workflows/%s/dispatches", cfg.Repo, cfg.Workflow),
		"-f", "ref=" + cfg.Ref,
		"-f", "return_run_details=true",
		"-f", "inputs[repo]=" + cfg.Repo,
		"-f", fmt.Sprintf("inputs[issue]=%d", cfg.IssueNumber),
		"-f", "inputs[agent]=" + cfg.AgentName,
		"-f", "inputs[session_id]=" + cfg.SessionID,
	}
	out, err := c.runner.Run(ctx, nil, args...)
	if err != nil {
		return dispatchResult{}, fmt.Errorf("gha: dispatch workflow %s: %w", cfg.Workflow, err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return dispatchResult{}, fmt.Errorf("gha: dispatch workflow %s did not return run details", cfg.Workflow)
	}
	var payload struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
		URL     string `json:"url"`
		LogsURL string `json:"logs_url"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return dispatchResult{}, fmt.Errorf("gha: parse dispatch response: %w", err)
	}
	if payload.ID == 0 {
		return dispatchResult{}, fmt.Errorf("gha: dispatch workflow %s did not return run id", cfg.Workflow)
	}
	return dispatchResult{
		RunID:   payload.ID,
		RunURL:  payload.HTMLURL,
		APIURL:  payload.URL,
		LogsURL: payload.LogsURL,
	}, nil
}

func (c *Client) waitForRun(ctx context.Context, cfg Config, dispatchedAt time.Time, dispatched dispatchResult) (Run, error) {
	if dispatched.RunID == 0 {
		return Run{}, fmt.Errorf("gha: workflow %s dispatch did not identify a run", cfg.Workflow)
	}
	for {
		select {
		case <-ctx.Done():
			return Run{}, fmt.Errorf("gha: poll workflow run %d: %w", dispatched.RunID, ctx.Err())
		default:
		}
		detail, err := c.getRun(ctx, cfg.Repo, dispatched.RunID)
		if err != nil {
			return Run{}, err
		}
		if detail.ID == 0 {
			return Run{}, fmt.Errorf("gha: workflow run %d returned empty metadata", dispatched.RunID)
		}
		run := detail
		if run.CreatedAt.IsZero() && !dispatchedAt.IsZero() {
			run.CreatedAt = dispatchedAt
		}
		if run.Status == "completed" {
			return run, nil
		}
		c.sleep(cfg.PollInterval)
	}
}

func (c *Client) getRun(ctx context.Context, repo string, runID int64) (Run, error) {
	out, err := c.runner.Run(ctx, nil, "api", fmt.Sprintf("repos/%s/actions/runs/%d", repo, runID))
	if err != nil {
		return Run{}, fmt.Errorf("gha: get run %d: %w", runID, err)
	}
	var payload struct {
		ID         int64  `json:"id"`
		HTMLURL    string `json:"html_url"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadBranch string `json:"head_branch"`
		CreatedAt  string `json:"created_at"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return Run{}, fmt.Errorf("gha: parse run %d: %w", runID, err)
	}
	createdAt, err := time.Parse(time.RFC3339, payload.CreatedAt)
	if err != nil {
		return Run{}, fmt.Errorf("gha: parse created_at for run %d: %w", runID, err)
	}
	return Run{
		ID:         payload.ID,
		HTMLURL:    payload.HTMLURL,
		Status:     payload.Status,
		Conclusion: payload.Conclusion,
		HeadBranch: payload.HeadBranch,
		CreatedAt:  createdAt,
	}, nil
}

func (c *Client) downloadLogs(ctx context.Context, cfg Config, runID int64) (string, string, error) {
	out, err := c.runner.Run(ctx, nil, "api", fmt.Sprintf("repos/%s/actions/runs/%d/logs", cfg.Repo, runID))
	if err != nil {
		return "", "", fmt.Errorf("gha: download logs for run %d: %w", runID, err)
	}
	logs, err := flattenZipText(out)
	if err != nil {
		return "", "", fmt.Errorf("gha: unzip logs for run %d: %w", runID, err)
	}
	logPath := filepath.Join(cfg.ArtifactDir, "gha-run.log")
	if err := os.WriteFile(logPath, []byte(logs), 0o644); err != nil {
		return "", "", fmt.Errorf("gha: write logs: %w", err)
	}
	return logPath, logs, nil
}

func (c *Client) downloadArtifacts(ctx context.Context, cfg Config, runID int64) ([]string, string, string, error) {
	out, err := c.runner.Run(ctx, nil, "api", fmt.Sprintf("repos/%s/actions/runs/%d/artifacts", cfg.Repo, runID))
	if err != nil {
		return nil, "", "", fmt.Errorf("gha: list artifacts for run %d: %w", runID, err)
	}
	var payload struct {
		Artifacts []struct {
			ID                 int64  `json:"id"`
			Name               string `json:"name"`
			ArchiveDownloadURL string `json:"archive_download_url"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, "", "", fmt.Errorf("gha: parse artifacts for run %d: %w", runID, err)
	}

	var files []string
	var resultPath string
	var canonicalSessionPath string
	for _, artifact := range payload.Artifacts {
		zipBody, err := c.runner.Run(ctx, nil, "api", artifact.ArchiveDownloadURL)
		if err != nil {
			return nil, "", "", fmt.Errorf("gha: download artifact %s: %w", artifact.Name, err)
		}
		extracted, err := unzipToDir(zipBody, filepath.Join(cfg.ArtifactDir, sanitizeName(artifact.Name)))
		if err != nil {
			return nil, "", "", fmt.Errorf("gha: unzip artifact %s: %w", artifact.Name, err)
		}
		files = append(files, extracted...)
	}

	sort.Strings(files)
	for _, path := range files {
		base := filepath.Base(path)
		switch base {
		case "workbuddy-result.json":
			resultPath = path
		case "events-v1.jsonl":
			canonicalSessionPath = path
		case "codex-exec.jsonl":
			if canonicalSessionPath == "" {
				canonicalSessionPath = path
			}
		}
	}
	if canonicalSessionPath == "" {
		return nil, "", "", fmt.Errorf("gha: run %d artifacts missing session capture (want events-v1.jsonl or codex-exec.jsonl)", runID)
	}
	return files, resultPath, canonicalSessionPath, nil
}

func flattenZipText(data []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		body, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr != nil {
			return "", readErr
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("== ")
		b.WriteString(f.Name)
		b.WriteString(" ==\n")
		b.Write(body)
		if len(body) == 0 || body[len(body)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

func unzipToDir(data []byte, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range r.File {
		target := filepath.Join(dir, filepath.Clean(f.Name))
		if !strings.HasPrefix(target, dir+string(os.PathSeparator)) && target != dir {
			return nil, fmt.Errorf("invalid zip path %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr != nil {
			return nil, readErr
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return nil, err
		}
		files = append(files, target)
	}
	return files, nil
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "artifact"
	}
	name = strings.ReplaceAll(name, "..", "")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	return name
}
