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

const defaultLogsPollInterval = 250 * time.Millisecond

type logsOpts struct {
	repo         string
	issue        int
	attempt      int
	stream       string
	follow       bool
	dbPath       string
	sessionsDir  string
	pollInterval time.Duration
}

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Print stored session logs for an issue attempt",
	Long:  "Resolve an issue run from the SQLite sessions index and print archived stdout, stderr, or tool-call artifacts.",
	RunE:  runLogsCmd,
}

func init() {
	logsCmd.Flags().String("repo", "", "Repository in OWNER/NAME form")
	logsCmd.Flags().Int("issue", 0, "Issue number to inspect")
	logsCmd.Flags().Int("attempt", 0, "Attempt number to inspect (1-based, default latest)")
	logsCmd.Flags().String("stream", "stdout", "Artifact stream to print: stdout, stderr, or tool-calls")
	logsCmd.Flags().BoolP("follow", "f", false, "Follow new log output for a running session")
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
	stream, _ := cmd.Flags().GetString("stream")
	follow, _ := cmd.Flags().GetBool("follow")
	dbPath, _ := cmd.Flags().GetString("db-path")

	repo = strings.TrimSpace(repo)
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
	if stream != "stdout" && stream != "stderr" && stream != "tool-calls" {
		return nil, fmt.Errorf("logs: --stream must be one of stdout, stderr, tool-calls")
	}
	if strings.TrimSpace(dbPath) == "" {
		return nil, fmt.Errorf("logs: --db-path is required")
	}

	return &logsOpts{
		repo:         repo,
		issue:        issue,
		attempt:      attempt,
		stream:       stream,
		follow:       follow,
		dbPath:       dbPath,
		pollInterval: defaultLogsPollInterval,
	}, nil
}

func runLogsWithOpts(ctx context.Context, opts *logsOpts, stdout io.Writer) error {
	dbPath, err := filepath.Abs(opts.dbPath)
	if err != nil {
		return fmt.Errorf("logs: resolve db path: %w", err)
	}
	st, err := store.NewStore(dbPath)
	if err != nil {
		return fmt.Errorf("logs: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	session, err := resolveLogsSession(st, opts.repo, opts.issue, opts.attempt)
	if err != nil {
		return err
	}

	sessionsDir := opts.sessionsDir
	if sessionsDir == "" {
		sessionsDir = filepath.Join(filepath.Dir(dbPath), "sessions")
	}
	eventsPath := filepath.Join(sessionsDir, session.SessionID, "events-v1.jsonl")
	if _, err := os.Stat(eventsPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("logs: events file not found for session %s: %s", session.SessionID, eventsPath)
		}
		return fmt.Errorf("logs: stat events file: %w", err)
	}

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

func resolveLogsSession(st *store.Store, repo string, issue, attempt int) (*store.AgentSession, error) {
	sessions, err := st.QueryAgentSessions(repo, issue)
	if err != nil {
		return nil, fmt.Errorf("logs: query sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("logs: no sessions found for %s issue #%d", repo, issue)
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
