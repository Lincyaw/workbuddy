package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

func TestParseLogsFlags(t *testing.T) {
	cmd := &cobra.Command{Use: "logs"}
	cmd.Flags().String("repo", "", "")
	cmd.Flags().Int("issue", 0, "")
	cmd.Flags().Int("attempt", 0, "")
	cmd.Flags().String("stream", "stdout", "")
	cmd.Flags().Bool("follow", false, "")
	cmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "")
	if err := cmd.Flags().Set("repo", "owner/name"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("issue", "48"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("stream", "tool-calls"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("attempt", "2"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("follow", "true"); err != nil {
		t.Fatal(err)
	}

	opts, err := parseLogsFlags(cmd)
	if err != nil {
		t.Fatalf("parseLogsFlags: %v", err)
	}
	if opts.repo != "owner/name" || opts.issue != 48 || opts.stream != "tool-calls" || opts.attempt != 2 || !opts.follow {
		t.Fatalf("unexpected opts: %+v", opts)
	}
}

func TestResolveLogsSession_DefaultsToLatestAttempt(t *testing.T) {
	fixture := newLogsFixture(t)

	session, err := resolveLogsSession(fixture.store, "owner/repo", 48, 0)
	if err != nil {
		t.Fatalf("resolveLogsSession: %v", err)
	}
	if session.SessionID != "session-002" {
		t.Fatalf("session = %q, want session-002", session.SessionID)
	}
}

func TestRunLogsWithOpts_FollowPrintsNewLines(t *testing.T) {
	fixture := newLogsFixture(t)
	path := filepath.Join(fixture.sessionsDir, "session-002", "events-v1.jsonl")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runLogsWithOpts(ctx, &logsOpts{
			repo:         "owner/repo",
			issue:        48,
			attempt:      2,
			stream:       "stdout",
			follow:       true,
			dbPath:       fixture.dbPath,
			pollInterval: 20 * time.Millisecond,
		}, &out)
	}()

	requireEventually(t, 2*time.Second, func() bool {
		return strings.Contains(out.String(), "attempt-2 start")
	})

	appendEvents(t, path,
		makeLogEvent(2, "session-002", "stdout", "follow one"),
		makeCommandOutputEvent(3, "session-002", "stdout", "follow two\n"),
	)
	if err := fixture.store.UpdateTaskStatus("task-002", store.TaskStatusCompleted); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLogsWithOpts: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runLogsWithOpts did not finish")
	}

	got := out.String()
	if !strings.Contains(got, "follow one") || !strings.Contains(got, "follow two") {
		t.Fatalf("stdout missing followed output: %q", got)
	}
}

func TestLogsCommand_E2E(t *testing.T) {
	fixture := newLogsFixture(t)
	repoRoot := repoRoot(t)

	cmd := exec.Command("go", "run", ".", "logs",
		"--repo", "owner/repo",
		"--issue", "48",
		"--attempt", "2",
		"--stream", "tool-calls",
		"--db-path", fixture.dbPath,
	)
	cmd.Dir = repoRoot
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("go run logs failed: %v\nstderr=%s", err, stderr.String())
	}

	got := stdout.String()
	if !strings.Contains(got, "\"kind\":\"command.exec\"") {
		t.Fatalf("stdout missing command.exec event: %q", got)
	}
	if !strings.Contains(got, "\"kind\":\"tool.call\"") {
		t.Fatalf("stdout missing tool.call event: %q", got)
	}
}

type logsFixture struct {
	dbPath      string
	sessionsDir string
	store       *store.Store
}

func newLogsFixture(t *testing.T) *logsFixture {
	t.Helper()

	root := t.TempDir()
	dbPath := filepath.Join(root, ".workbuddy", "workbuddy.db")
	sessionsDir := filepath.Join(root, ".workbuddy", "sessions")

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	insertSessionFixture(t, st, "task-001", "session-001", store.TaskStatusCompleted)
	insertSessionFixture(t, st, "task-002", "session-002", store.TaskStatusRunning)

	writeEventsFile(t, filepath.Join(sessionsDir, "session-001", "events-v1.jsonl"),
		makeLogEvent(1, "session-001", "stdout", "attempt-1 start"),
		makeLogEvent(2, "session-001", "stderr", "attempt-1 err"),
	)
	writeEventsFile(t, filepath.Join(sessionsDir, "session-002", "events-v1.jsonl"),
		makeLogEvent(1, "session-002", "stdout", "attempt-2 start"),
		makeCommandExecEvent(2, "session-002", "cmd-1", []string{"bash", "-lc", "echo hi"}),
		makeToolCallEvent(3, "session-002", "tool-1", "github.search"),
	)

	return &logsFixture{dbPath: dbPath, sessionsDir: sessionsDir, store: st}
}

func insertSessionFixture(t *testing.T, st *store.Store, taskID, sessionID, status string) {
	t.Helper()
	if err := st.InsertTask(store.TaskRecord{
		ID:        taskID,
		Repo:      "owner/repo",
		IssueNum:  48,
		AgentName: "dev-agent",
		Status:    status,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := st.InsertAgentSession(store.AgentSession{
		SessionID: sessionID,
		TaskID:    taskID,
		Repo:      "owner/repo",
		IssueNum:  48,
		AgentName: "dev-agent",
	}); err != nil {
		t.Fatalf("InsertAgentSession: %v", err)
	}
}

func writeEventsFile(t *testing.T, path string, events ...launcherevents.Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	appendEvents(t, path, events...)
}

func appendEvents(t *testing.T, path string, events ...launcherevents.Event) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = f.Close() }()

	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
}

func makeLogEvent(seq uint64, sessionID, stream, line string) launcherevents.Event {
	return launcherevents.Event{
		Kind:      launcherevents.KindLog,
		Timestamp: time.Date(2026, 4, 16, 9, 0, 0, 0, time.UTC),
		SessionID: sessionID,
		Seq:       seq,
		Payload:   launcherevents.MustPayload(launcherevents.LogPayload{Stream: stream, Line: line}),
	}
}

func makeCommandOutputEvent(seq uint64, sessionID, stream, data string) launcherevents.Event {
	return launcherevents.Event{
		Kind:      launcherevents.KindCommandOutput,
		Timestamp: time.Date(2026, 4, 16, 9, 0, 0, 0, time.UTC),
		SessionID: sessionID,
		Seq:       seq,
		Payload:   launcherevents.MustPayload(launcherevents.CommandOutputPayload{CallID: "cmd-1", Stream: stream, Data: data}),
	}
}

func makeCommandExecEvent(seq uint64, sessionID, callID string, cmd []string) launcherevents.Event {
	return launcherevents.Event{
		Kind:      launcherevents.KindCommandExec,
		Timestamp: time.Date(2026, 4, 16, 9, 0, 0, 0, time.UTC),
		SessionID: sessionID,
		Seq:       seq,
		Payload:   launcherevents.MustPayload(launcherevents.CommandExecPayload{CallID: callID, Cmd: cmd}),
	}
}

func makeToolCallEvent(seq uint64, sessionID, callID, name string) launcherevents.Event {
	return launcherevents.Event{
		Kind:      launcherevents.KindToolCall,
		Timestamp: time.Date(2026, 4, 16, 9, 0, 0, 0, time.UTC),
		SessionID: sessionID,
		Seq:       seq,
		Payload:   launcherevents.MustPayload(launcherevents.ToolCallPayload{CallID: callID, Name: name}),
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Dir(wd)
}

func requireEventually(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
