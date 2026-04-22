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

func TestRunLogsWithOpts_DefaultSummaryTextOutput(t *testing.T) {
	fixture := newLogsFixture(t)

	var out bytes.Buffer
	if err := runLogsWithOpts(context.Background(), &logsOpts{
		repo:   "owner/repo",
		issue:  48,
		dbPath: fixture.dbPath,
		view:   "summary",
		format: "text",
	}, &out); err != nil {
		t.Fatalf("runLogsWithOpts: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"repo: owner/repo",
		"issue: 48",
		"attempt: 2",
		"session: session-002",
		"agent: dev-agent",
		"status: running",
		"created_at:",
		"[2] command.exec: bash -lc echo hi",
		"[3] tool.call: github.search",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary text missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "attempt-2 start") {
		t.Fatalf("summary text should not replay raw stdout: %q", got)
	}
}

func TestRunLogsWithOpts_SummaryJSONOutput(t *testing.T) {
	fixture := newLogsFixture(t)

	var out bytes.Buffer
	if err := runLogsWithOpts(context.Background(), &logsOpts{
		repo:    "owner/repo",
		issue:   48,
		attempt: 2,
		dbPath:  fixture.dbPath,
		view:    "summary",
		format:  "json",
	}, &out); err != nil {
		t.Fatalf("runLogsWithOpts: %v", err)
	}

	var summary logsSummary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("json.Unmarshal: %v\nbody=%s", err, out.String())
	}
	if summary.Repo != "owner/repo" || summary.Issue != 48 || summary.Attempt != 2 {
		t.Fatalf("unexpected summary header: %+v", summary)
	}
	if summary.SessionID != "session-002" || summary.AgentName != "dev-agent" || summary.TaskStatus != store.TaskStatusRunning {
		t.Fatalf("unexpected summary session: %+v", summary)
	}
	if len(summary.RecentEvents) < 2 {
		t.Fatalf("recent events too short: %+v", summary.RecentEvents)
	}
	if summary.RecentEvents[0].Kind != string(launcherevents.KindCommandExec) {
		t.Fatalf("first recent event = %+v", summary.RecentEvents[0])
	}
}

func TestRunLogsWithOpts_ArtifactToolCallsStillWork(t *testing.T) {
	fixture := newLogsFixture(t)

	var out bytes.Buffer
	if err := runLogsWithOpts(context.Background(), &logsOpts{
		repo:    "owner/repo",
		issue:   48,
		attempt: 2,
		dbPath:  fixture.dbPath,
		view:    "artifact",
		stream:  "tool-calls",
	}, &out); err != nil {
		t.Fatalf("runLogsWithOpts: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, `"kind":"command.exec"`) {
		t.Fatalf("artifact output missing command.exec: %q", got)
	}
	if !strings.Contains(got, `"kind":"tool.call"`) {
		t.Fatalf("artifact output missing tool.call: %q", got)
	}
}

func TestParseLogsFlags_RejectsInvalidViewStreamCombination(t *testing.T) {
	cmd := &cobra.Command{Use: "logs"}
	cmd.Flags().String("repo", "", "")
	cmd.Flags().Int("issue", 0, "")
	cmd.Flags().Int("attempt", 0, "")
	cmd.Flags().String("view", "summary", "")
	cmd.Flags().String("format", "text", "")
	cmd.Flags().String("stream", "stdout", "")
	cmd.Flags().Bool("follow", false, "")
	cmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "")

	_ = cmd.Flags().Set("repo", "owner/repo")
	_ = cmd.Flags().Set("issue", "48")
	_ = cmd.Flags().Set("stream", "stderr")

	_, err := parseLogsFlags(cmd)
	if err == nil || !strings.Contains(err.Error(), "--stream is only valid with --view artifact") {
		t.Fatalf("parseLogsFlags error = %v, want invalid stream/view diagnostic", err)
	}
}

func TestParseLogsFlags_RejectsInvalidArtifactFormatCombination(t *testing.T) {
	cmd := &cobra.Command{Use: "logs"}
	cmd.Flags().String("repo", "", "")
	cmd.Flags().Int("issue", 0, "")
	cmd.Flags().Int("attempt", 0, "")
	cmd.Flags().String("view", "summary", "")
	cmd.Flags().String("format", "text", "")
	cmd.Flags().String("stream", "stdout", "")
	cmd.Flags().Bool("follow", false, "")
	cmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "")

	_ = cmd.Flags().Set("repo", "owner/repo")
	_ = cmd.Flags().Set("issue", "48")
	_ = cmd.Flags().Set("view", "artifact")
	_ = cmd.Flags().Set("format", "json")

	_, err := parseLogsFlags(cmd)
	if err == nil || !strings.Contains(err.Error(), "--format is only valid with --view summary") {
		t.Fatalf("parseLogsFlags error = %v, want invalid format/view diagnostic", err)
	}
}

func TestRunLogsWithOpts_FollowSummaryPrintsFinalState(t *testing.T) {
	fixture := newLogsFixture(t)
	path := filepath.Join(fixture.sessionsDir, "session-002", "events-v1.jsonl")

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runLogsWithOpts(context.Background(), &logsOpts{
			repo:         "owner/repo",
			issue:        48,
			attempt:      2,
			dbPath:       fixture.dbPath,
			view:         "summary",
			format:       "text",
			follow:       true,
			pollInterval: 20 * time.Millisecond,
		}, &out)
	}()

	time.Sleep(60 * time.Millisecond)
	appendEvents(t, path,
		makeCommandOutputEvent(4, "session-002", "stderr", "fatal: branch already exists\n"),
		makeTaskCompleteEvent(5, "session-002", "completed"),
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
	if !strings.Contains(got, "status: completed") {
		t.Fatalf("follow summary missing final status: %q", got)
	}
	if !strings.Contains(got, "[4] command.output: fatal: branch already exists") {
		t.Fatalf("follow summary missing bounded recent event: %q", got)
	}
	if strings.Count(got, "repo: owner/repo") != 1 {
		t.Fatalf("follow summary should print one final summary: %q", got)
	}
}

func TestRunLogsWithOpts_UsesManagedWorkerDeployment(t *testing.T) {
	repoDir := t.TempDir()
	workerDir := t.TempDir()
	configHome := filepath.Join(t.TempDir(), ".config")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", configHome)

	dbPath := filepath.Join(workerDir, ".workbuddy", "worker.db")
	sessionsDir := filepath.Join(workerDir, ".workbuddy", "sessions")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	insertSessionFixture(t, st, "task-001", "session-001", store.TaskStatusCompleted)
	writeEventsFile(t, filepath.Join(sessionsDir, "session-001", "events-v1.jsonl"),
		makeCommandExecEvent(1, "session-001", "cmd-1", []string{"bash", "-lc", "echo managed"}),
	)

	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "workbuddy-worker",
		Scope:            "user",
		BinaryPath:       "/tmp/workbuddy",
		WorkingDirectory: workerDir,
		Command:          []string{"worker", "--coordinator", "http://127.0.0.1:8081", "--repos", "owner/repo=" + repoDir},
	}
	manifestPath := filepath.Join(configHome, "workbuddy", "deployments", "workbuddy-worker.json")
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("writeDeploymentManifest: %v", err)
	}

	prevWD, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("Abs(.): %v", err)
	}
	if err := os.Chdir(repoDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})

	var out bytes.Buffer
	if err := runLogsWithOpts(context.Background(), &logsOpts{
		repo:   "owner/repo",
		issue:  48,
		view:   "summary",
		format: "text",
		dbPath: ".workbuddy/workbuddy.db",
	}, &out); err != nil {
		t.Fatalf("runLogsWithOpts: %v", err)
	}
	if !strings.Contains(out.String(), "echo managed") {
		t.Fatalf("summary missing managed worker command: %q", out.String())
	}
}

func TestLogsCommand_E2E(t *testing.T) {
	fixture := newLogsFixture(t)
	repoRoot := repoRoot(t)

	cmd := exec.Command("go", "run", ".", "logs",
		"--repo", "owner/repo",
		"--issue", "48",
		"--attempt", "2",
		"--view", "artifact",
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

func makeTaskCompleteEvent(seq uint64, sessionID, status string) launcherevents.Event {
	return launcherevents.Event{
		Kind:      launcherevents.KindTaskComplete,
		Timestamp: time.Date(2026, 4, 16, 9, 0, 0, 0, time.UTC),
		SessionID: sessionID,
		Seq:       seq,
		Payload:   launcherevents.MustPayload(launcherevents.TaskCompletePayload{TurnID: "turn-1", Status: status}),
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
