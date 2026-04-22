package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

const (
	defaultLogsPollInterval = 250 * time.Millisecond
	defaultLogsSummaryLimit = 8
	defaultLogsLineLimit    = 120
)

var errLogsSessionNotFound = errors.New("logs: no sessions found")

type logsOpts struct {
	repo         string
	issue        int
	attempt      int
	view         string
	format       string
	stream       string
	streamSet    bool
	formatSet    bool
	follow       bool
	dbPath       string
	dbPathSet    bool
	sessionsDir  string
	pollInterval time.Duration
}

type logsSummary struct {
	Repo         string             `json:"repo"`
	Issue        int                `json:"issue"`
	Attempt      int                `json:"attempt"`
	SessionID    string             `json:"session_id"`
	AgentName    string             `json:"agent_name"`
	TaskStatus   string             `json:"task_status"`
	CreatedAt    string             `json:"created_at"`
	RecentEvents []logsSummaryEvent `json:"recent_events"`
}

type logsSummaryEvent struct {
	Seq     uint64 `json:"seq"`
	Kind    string `json:"kind"`
	Time    string `json:"time,omitempty"`
	Summary string `json:"summary"`
}

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Inspect a stored session for an issue attempt",
	Long:  "Resolve an issue run from the SQLite sessions index and print either a concise session summary or an explicit archived artifact stream.",
	Example: strings.Join([]string{
		"  workbuddy logs --issue 78 --repo owner/name",
		"  workbuddy logs --issue 78 --repo owner/name --format json",
		"  workbuddy logs --issue 78 --repo owner/name --view artifact --stream tool-calls",
	}, "\n"),
	RunE:  runLogsCmd,
}

func init() {
	logsCmd.Flags().String("repo", "", "Repository in OWNER/NAME form")
	logsCmd.Flags().Int("issue", 0, "Issue number to inspect")
	logsCmd.Flags().Int("attempt", 0, "Attempt number to inspect (1-based, default latest)")
	logsCmd.Flags().String("view", "summary", "Output view: summary or artifact")
	logsCmd.Flags().String("format", "text", "Output format: text or json")
	logsCmd.Flags().String("stream", "stdout", "Artifact stream to print when --view artifact: stdout, stderr, or tool-calls")
	logsCmd.Flags().BoolP("follow", "f", false, "Follow a running session until it completes")
	logsCmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path")
	rootCmd.AddCommand(logsCmd)
}

func runLogsCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseLogsFlags(cmd)
	if err != nil {
		return err
	}
	return runLogsWithOpts(cmd.Context(), opts, os.Stdout)
}

func parseLogsFlags(cmd *cobra.Command) (*logsOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	issue, _ := cmd.Flags().GetInt("issue")
	attempt, _ := cmd.Flags().GetInt("attempt")
	view, _ := cmd.Flags().GetString("view")
	format, _ := cmd.Flags().GetString("format")
	stream, _ := cmd.Flags().GetString("stream")
	follow, _ := cmd.Flags().GetBool("follow")
	dbPath, _ := cmd.Flags().GetString("db-path")
	dbPathSet := cmd.Flags().Changed("db-path")
	streamSet := cmd.Flags().Changed("stream")
	formatSet := cmd.Flags().Changed("format")

	repo = strings.TrimSpace(repo)
	view = strings.TrimSpace(view)
	format = strings.TrimSpace(format)
	stream = strings.TrimSpace(stream)

	if repo == "" {
		return nil, fmt.Errorf("logs: --repo is required")
	}
	if issue <= 0 {
		return nil, fmt.Errorf("logs: --issue must be > 0")
	}
	if attempt < 0 {
		return nil, fmt.Errorf("logs: --attempt must be >= 0")
	}
	if view != "summary" && view != "artifact" {
		return nil, fmt.Errorf("logs: --view must be one of summary, artifact")
	}
	if format != "text" && format != "json" {
		return nil, fmt.Errorf("logs: --format must be one of text, json")
	}
	if stream != "stdout" && stream != "stderr" && stream != "tool-calls" {
		return nil, fmt.Errorf("logs: --stream must be one of stdout, stderr, tool-calls")
	}
	if view == "summary" && streamSet {
		return nil, fmt.Errorf("logs: --stream is only valid with --view artifact")
	}
	if view == "artifact" && formatSet {
		return nil, fmt.Errorf("logs: --format is only valid with --view summary")
	}
	if strings.TrimSpace(dbPath) == "" {
		return nil, fmt.Errorf("logs: --db-path is required")
	}

	return &logsOpts{
		repo:         repo,
		issue:        issue,
		attempt:      attempt,
		view:         view,
		format:       format,
		stream:       stream,
		streamSet:    streamSet,
		formatSet:    formatSet,
		follow:       follow,
		dbPath:       dbPath,
		dbPathSet:    dbPathSet,
		pollInterval: defaultLogsPollInterval,
	}, nil
}

func runLogsWithOpts(ctx context.Context, opts *logsOpts, stdout io.Writer) error {
	st, session, resolved, err := resolveLogsStoreAndSession(opts)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	sessionsDir := opts.sessionsDir
	if sessionsDir == "" {
		sessionsDir = resolved.sessionsDir
	}
	eventsPath := filepath.Join(sessionsDir, session.SessionID, "events-v1.jsonl")
	if _, err := os.Stat(eventsPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("logs: events file not found for session %s: %s", session.SessionID, eventsPath)
		}
		return fmt.Errorf("logs: stat events file: %w", err)
	}

	if opts.view == "artifact" {
		return runArtifactLogs(ctx, opts, stdout, st, session, eventsPath)
	}
	return runSummaryLogs(ctx, opts, stdout, st, session, eventsPath)
}

func runArtifactLogs(ctx context.Context, opts *logsOpts, stdout io.Writer, st *store.Store, session *store.AgentSession, eventsPath string) error {
	streamer := newLogsStreamer(opts.stream, stdout)
	offset, err := streamer.drainFile(eventsPath)
	if err != nil {
		return err
	}
	if !opts.follow || !isSessionRunning(session.TaskStatus) {
		return nil
	}

	if opts.pollInterval <= 0 {
		opts.pollInterval = defaultLogsPollInterval
	}
	ticker := time.NewTicker(opts.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			offset, err = streamer.followFrom(eventsPath, offset)
			if err != nil {
				return err
			}
			current, err := st.GetAgentSession(session.SessionID)
			if err != nil {
				return fmt.Errorf("logs: refresh session status: %w", err)
			}
			if current == nil || !isSessionRunning(current.TaskStatus) {
				return nil
			}
		}
	}
}

func runSummaryLogs(ctx context.Context, opts *logsOpts, stdout io.Writer, st *store.Store, session *store.AgentSession, eventsPath string) error {
	current := session
	if opts.follow && isSessionRunning(session.TaskStatus) {
		if opts.pollInterval <= 0 {
			opts.pollInterval = defaultLogsPollInterval
		}
		ticker := time.NewTicker(opts.pollInterval)
		defer ticker.Stop()
		for isSessionRunning(current.TaskStatus) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				fresh, err := st.GetAgentSession(session.SessionID)
				if err != nil {
					return fmt.Errorf("logs: refresh session status: %w", err)
				}
				if fresh == nil {
					return fmt.Errorf("logs: session %s disappeared during follow", session.SessionID)
				}
				current = fresh
			}
		}
	}

	summary, err := buildLogsSummary(eventsPath, current, opts.repo, opts.issue, resolveAttemptNumber(st, current, opts.issue, opts.attempt))
	if err != nil {
		return err
	}
	return writeLogsSummary(stdout, summary, opts.format)
}

func resolveAttemptNumber(st *store.Store, session *store.AgentSession, issue, requested int) int {
	if session == nil {
		return requested
	}
	sessions, err := st.QueryAgentSessions(session.Repo, issue)
	if err != nil || len(sessions) == 0 {
		if requested > 0 {
			return requested
		}
		return 1
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].CreatedAt.Before(sessions[j].CreatedAt)
	})
	for idx := range sessions {
		if sessions[idx].SessionID == session.SessionID {
			return idx + 1
		}
	}
	if requested > 0 {
		return requested
	}
	return len(sessions)
}

func writeLogsSummary(w io.Writer, summary *logsSummary, format string) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	}

	if _, err := fmt.Fprintf(w,
		"repo: %s\nissue: %d\nattempt: %d\nsession: %s\nagent: %s\nstatus: %s\ncreated_at: %s\n",
		summary.Repo,
		summary.Issue,
		summary.Attempt,
		summary.SessionID,
		summary.AgentName,
		summary.TaskStatus,
		summary.CreatedAt,
	); err != nil {
		return err
	}
	if len(summary.RecentEvents) == 0 {
		_, err := fmt.Fprintln(w, "recent_events: none")
		return err
	}
	if _, err := fmt.Fprintln(w, "recent_events:"); err != nil {
		return err
	}
	for _, event := range summary.RecentEvents {
		if _, err := fmt.Fprintf(w, "- [%d] %s: %s\n", event.Seq, event.Kind, event.Summary); err != nil {
			return err
		}
	}
	return nil
}

func buildLogsSummary(eventsPath string, session *store.AgentSession, repo string, issue, attempt int) (*logsSummary, error) {
	if session == nil {
		return nil, fmt.Errorf("logs: session metadata unavailable")
	}
	file, err := os.Open(eventsPath)
	if err != nil {
		return nil, fmt.Errorf("logs: open events file: %w", err)
	}
	defer func() { _ = file.Close() }()

	recent := make([]logsSummaryEvent, 0, defaultLogsSummaryLimit)
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		var event launcherevents.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if item, ok := summarizeLogsEvent(&event); ok {
			recent = append(recent, item)
			if len(recent) > defaultLogsSummaryLimit {
				recent = recent[len(recent)-defaultLogsSummaryLimit:]
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("logs: read events file: %w", err)
	}

	return &logsSummary{
		Repo:         repo,
		Issue:        issue,
		Attempt:      attempt,
		SessionID:    session.SessionID,
		AgentName:    session.AgentName,
		TaskStatus:   session.TaskStatus,
		CreatedAt:    session.CreatedAt.UTC().Format(time.RFC3339),
		RecentEvents: recent,
	}, nil
}

func summarizeLogsEvent(event *launcherevents.Event) (logsSummaryEvent, bool) {
	item := logsSummaryEvent{Seq: event.Seq, Kind: string(event.Kind)}
	if !event.Timestamp.IsZero() {
		item.Time = event.Timestamp.UTC().Format(time.RFC3339)
	}

	switch event.Kind {
	case launcherevents.KindCommandExec:
		var payload launcherevents.CommandExecPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return logsSummaryEvent{}, false
		}
		item.Summary = truncateLogsText(strings.Join(payload.Cmd, " "))
		return item, item.Summary != ""
	case launcherevents.KindToolCall:
		var payload launcherevents.ToolCallPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return logsSummaryEvent{}, false
		}
		item.Summary = truncateLogsText(payload.Name)
		return item, item.Summary != ""
	case launcherevents.KindToolResult:
		var payload launcherevents.ToolResultPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.OK {
			return logsSummaryEvent{}, false
		}
		item.Summary = truncateLogsText(extractPayloadSummary(payload.Result))
		if item.Summary == "" {
			item.Summary = "tool call failed"
		}
		return item, true
	case launcherevents.KindLog:
		var payload launcherevents.LogPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return logsSummaryEvent{}, false
		}
		if payload.Stream != "stderr" && !looksLikeImportantLogLine(payload.Line) {
			return logsSummaryEvent{}, false
		}
		item.Summary = truncateLogsText(payload.Line)
		return item, item.Summary != ""
	case launcherevents.KindCommandOutput:
		var payload launcherevents.CommandOutputPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return logsSummaryEvent{}, false
		}
		if payload.Stream != "stderr" && !looksLikeImportantLogLine(payload.Data) {
			return logsSummaryEvent{}, false
		}
		item.Summary = truncateLogsText(payload.Data)
		return item, item.Summary != ""
	case launcherevents.KindTaskComplete:
		var payload launcherevents.TaskCompletePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return logsSummaryEvent{}, false
		}
		item.Summary = truncateLogsText("status=" + payload.Status)
		return item, true
	case launcherevents.KindTurnCompleted:
		var payload launcherevents.TurnCompletedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return logsSummaryEvent{}, false
		}
		item.Summary = truncateLogsText("status=" + payload.Status)
		return item, true
	case launcherevents.KindError:
		var payload launcherevents.ErrorPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return logsSummaryEvent{}, false
		}
		item.Summary = truncateLogsText(payload.Code + ": " + payload.Message)
		return item, item.Summary != ""
	default:
		return logsSummaryEvent{}, false
	}
}

func looksLikeImportantLogLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if lower == "" {
		return false
	}
	for _, needle := range []string{"error", "warn", "fail", "panic", "fatal", "denied"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func truncateLogsText(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if s == "" {
		return ""
	}
	if len(s) <= defaultLogsLineLimit {
		return s
	}
	return strings.TrimSpace(s[:defaultLogsLineLimit-3]) + "..."
}

func extractPayloadSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err == nil {
		data, err := json.Marshal(generic)
		if err == nil {
			return string(data)
		}
	}
	return string(raw)
}

type logsStoreTarget struct {
	dbPath      string
	sessionsDir string
}

func resolveLogsStoreAndSession(opts *logsOpts) (*store.Store, *store.AgentSession, logsStoreTarget, error) {
	candidates := logsStoreCandidates(opts)
	var artifactErr error
	var sessionErr error
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.dbPath) == "" {
			continue
		}
		dbPath, err := filepath.Abs(candidate.dbPath)
		if err != nil {
			continue
		}
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		st, err := store.NewStore(dbPath)
		if err != nil {
			continue
		}
		session, err := resolveLogsSession(st, opts.repo, opts.issue, opts.attempt)
		if err != nil {
			_ = st.Close()
			if errors.Is(err, errLogsSessionNotFound) {
				sessionErr = err
				continue
			}
			return nil, nil, logsStoreTarget{}, err
		}
		eventsPath := filepath.Join(candidate.sessionsDir, session.SessionID, "events-v1.jsonl")
		if _, err := os.Stat(eventsPath); err == nil {
			candidate.dbPath = dbPath
			return st, session, candidate, nil
		} else if errors.Is(err, os.ErrNotExist) {
			artifactErr = fmt.Errorf("logs: events file not found for session %s: %s", session.SessionID, eventsPath)
			_ = st.Close()
			continue
		} else {
			_ = st.Close()
			return nil, nil, logsStoreTarget{}, fmt.Errorf("logs: stat events file: %w", err)
		}
	}
	if artifactErr != nil {
		return nil, nil, logsStoreTarget{}, artifactErr
	}
	if sessionErr != nil {
		return nil, nil, logsStoreTarget{}, sessionErr
	}
	return nil, nil, logsStoreTarget{}, fmt.Errorf("%w for %s issue #%d", errLogsSessionNotFound, opts.repo, opts.issue)
}

func logsStoreCandidates(opts *logsOpts) []logsStoreTarget {
	var candidates []logsStoreTarget
	if !opts.dbPathSet && strings.TrimSpace(opts.repo) != "" {
		if managed, ok := discoverManagedLogsTarget(opts.repo); ok {
			candidates = append(candidates, *managed)
		}
	}
	if strings.TrimSpace(opts.dbPath) != "" {
		candidates = append(candidates, logsStoreTarget{
			dbPath:      strings.TrimSpace(opts.dbPath),
			sessionsDir: deriveLogsSessionsDir(opts.sessionsDir, strings.TrimSpace(opts.dbPath)),
		})
	}
	if !opts.dbPathSet && strings.TrimSpace(opts.sessionsDir) == "" {
		primary := strings.TrimSpace(opts.dbPath)
		if filepath.Base(primary) == "workbuddy.db" {
			candidates = append(candidates, logsStoreTarget{
				dbPath:      filepath.Join(filepath.Dir(primary), "worker.db"),
				sessionsDir: filepath.Join(filepath.Dir(primary), "sessions"),
			})
		}
	}
	return uniqueLogsStoreTargets(candidates)
}

func deriveLogsSessionsDir(explicit, dbPath string) string {
	if strings.TrimSpace(explicit) != "" {
		return strings.TrimSpace(explicit)
	}
	return filepath.Join(filepath.Dir(dbPath), "sessions")
}

func uniqueLogsStoreTargets(candidates []logsStoreTarget) []logsStoreTarget {
	seen := make(map[string]bool, len(candidates))
	out := make([]logsStoreTarget, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.dbPath) == "" {
			continue
		}
		dbPath, err := filepath.Abs(candidate.dbPath)
		if err != nil {
			dbPath = candidate.dbPath
		}
		sessionsDir := strings.TrimSpace(candidate.sessionsDir)
		if sessionsDir == "" {
			sessionsDir = deriveLogsSessionsDir("", dbPath)
		}
		key := dbPath + "\x00" + sessionsDir
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, logsStoreTarget{dbPath: dbPath, sessionsDir: sessionsDir})
	}
	return out
}

func discoverManagedLogsTarget(repo string) (*logsStoreTarget, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	manifestDirs := make([]string, 0, 2)
	for _, scope := range []string{"user", "system"} {
		_, paths, err := resolveDeployScopePaths(scope)
		if err != nil || paths == nil || strings.TrimSpace(paths.manifestDir) == "" {
			continue
		}
		manifestDirs = append(manifestDirs, paths.manifestDir)
	}

	var (
		best      *logsStoreTarget
		bestScore = -1
	)
	for _, manifestDir := range manifestDirs {
		entries, err := filepath.Glob(filepath.Join(manifestDir, "*.json"))
		if err != nil {
			continue
		}
		for _, manifestPath := range entries {
			manifest, err := readDeploymentManifest(manifestPath)
			if err != nil {
				continue
			}
			target, score := logsTargetFromManifest(manifest, repo, cwd)
			if score < 0 || target == nil {
				continue
			}
			if best == nil || score > bestScore {
				best = target
				bestScore = score
			}
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

func logsTargetFromManifest(manifest *deploymentManifest, repo, cwd string) (*logsStoreTarget, int) {
	if manifest == nil {
		return nil, -1
	}
	args := deploymentCommandArgs(manifest)
	runtime := strings.TrimSpace(args[0])
	if runtime == "" {
		runtime = "serve"
	}
	var (
		dbPath string
		score  int
	)
	switch runtime {
	case "worker":
		dbPath = filepath.Join(manifest.WorkingDirectory, ".workbuddy", "worker.db")
		score = 45
	case "serve":
		dbPath = strings.TrimSpace(dbPathFromCommand(manifest, runtime, args[1:]))
		if dbPath == "" {
			return nil, -1
		}
		score = 35
	default:
		return nil, -1
	}
	if repo != "" && manifestMatchesStatusRepo(manifest, runtime, repo, cwd) {
		score += 100
	}
	if cwd != "" && strings.TrimSpace(manifest.WorkingDirectory) != "" && pathWithin(manifest.WorkingDirectory, cwd) {
		score += 40
	}
	return &logsStoreTarget{
		dbPath:      dbPath,
		sessionsDir: filepath.Join(manifest.WorkingDirectory, ".workbuddy", "sessions"),
	}, score
}

func dbPathFromCommand(manifest *deploymentManifest, runtime string, args []string) string {
	switch runtime {
	case "serve", "coordinator":
		value, ok := commandFlagValue(args, "--db")
		if !ok || strings.TrimSpace(value) == "" {
			value = ".workbuddy/workbuddy.db"
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(manifest.WorkingDirectory, value)
		}
		return value
	default:
		return ""
	}
}

func resolveLogsSession(st *store.Store, repo string, issue, attempt int) (*store.AgentSession, error) {
	sessions, err := st.QueryAgentSessions(repo, issue)
	if err != nil {
		return nil, fmt.Errorf("logs: query sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("%w for %s issue #%d", errLogsSessionNotFound, repo, issue)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].CreatedAt.Before(sessions[j].CreatedAt)
	})
	if attempt == 0 {
		session := sessions[len(sessions)-1]
		return &session, nil
	}
	if attempt > len(sessions) {
		return nil, fmt.Errorf("logs: attempt %d not found for %s issue #%d", attempt, repo, issue)
	}
	session := sessions[attempt-1]
	return &session, nil
}

func isSessionRunning(status string) bool {
	return status == store.TaskStatusPending || status == store.TaskStatusRunning
}

type logsStreamer struct {
	stream string
	out    io.Writer
}

func newLogsStreamer(stream string, out io.Writer) *logsStreamer {
	return &logsStreamer{stream: stream, out: out}
}

func (s *logsStreamer) drainFile(path string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("logs: open events file: %w", err)
	}
	defer func() { _ = file.Close() }()

	offset, err := s.readFrom(file, 0)
	if err != nil {
		return 0, err
	}
	return offset, nil
}

func (s *logsStreamer) followFrom(path string, offset int64) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return offset, fmt.Errorf("logs: open events file: %w", err)
	}
	defer func() { _ = file.Close() }()

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, fmt.Errorf("logs: seek events file: %w", err)
	}
	return s.readFrom(file, offset)
}

func (s *logsStreamer) readFrom(file *os.File, start int64) (int64, error) {
	reader := bufio.NewReader(file)
	offset := start
	for {
		line, err := reader.ReadBytes('\n')
		offset += int64(len(line))
		if len(line) > 0 {
			trimmed := strings.TrimRight(string(line), "\r\n")
			if trimmed != "" {
				if err := s.writeEventLine(trimmed); err != nil {
					return offset, err
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return offset, nil
			}
			return offset, fmt.Errorf("logs: read events file: %w", err)
		}
	}
}

func (s *logsStreamer) writeEventLine(line string) error {
	var event launcherevents.Event
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return nil
	}
	switch s.stream {
	case "stdout":
		return s.writeStdStreamEvent(&event, "stdout")
	case "stderr":
		return s.writeStdStreamEvent(&event, "stderr")
	case "tool-calls":
		return s.writeToolEvent(&event)
	default:
		return nil
	}
}

func (s *logsStreamer) writeStdStreamEvent(event *launcherevents.Event, stream string) error {
	switch event.Kind {
	case launcherevents.KindLog:
		var payload launcherevents.LogPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Stream != stream {
			return nil
		}
		_, err := fmt.Fprintln(s.out, payload.Line)
		return err
	case launcherevents.KindCommandOutput:
		var payload launcherevents.CommandOutputPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Stream != stream {
			return nil
		}
		_, err := io.WriteString(s.out, payload.Data)
		return err
	default:
		return nil
	}
}

func (s *logsStreamer) writeToolEvent(event *launcherevents.Event) error {
	switch event.Kind {
	case launcherevents.KindToolCall, launcherevents.KindToolResult, launcherevents.KindCommandExec:
	default:
		return nil
	}

	record := map[string]any{
		"kind": event.Kind,
		"seq":  event.Seq,
	}
	if !event.Timestamp.IsZero() {
		record["ts"] = event.Timestamp.Format(time.RFC3339Nano)
	}
	if event.SessionID != "" {
		record["session_id"] = event.SessionID
	}
	if event.TurnID != "" {
		record["turn_id"] = event.TurnID
	}

	var payload any
	if len(event.Payload) > 0 {
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			payload = string(event.Payload)
		}
	}
	record["payload"] = payload

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("logs: marshal tool event: %w", err)
	}
	_, err = fmt.Fprintln(s.out, string(data))
	return err
}
