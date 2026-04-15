package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/launcher"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// helper: create a temp store + auditor
func setup(t *testing.T) (*Auditor, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sessionsDir := filepath.Join(tmpDir, "sessions")
	aud := NewAuditor(s, sessionsDir)
	return aud, tmpDir
}

// Scenario 1: Claude Code session capture
func TestCapture_ClaudeCode(t *testing.T) {
	aud, tmpDir := setup(t)

	// Create a mock Claude Code session JSON file.
	sessionData := claudeSession{
		Messages: []claudeMessage{
			{
				Role: "assistant",
				Content: mustJSON(t, []claudeContentBlock{
					{Type: "text", Text: "I will fix the bug."},
					{Type: "tool_use", Name: "write_file"},
				}),
			},
			{
				Role: "assistant",
				Content: mustJSON(t, []claudeContentBlock{
					{Type: "tool_use", Name: "read_file"},
					{Type: "tool_use", Name: "write_file"},
					{Type: "text", Text: "Done."},
				}),
			},
		},
		Usage: map[string]interface{}{
			"input_tokens":  1500,
			"output_tokens": 800,
		},
	}

	sessionFile := filepath.Join(tmpDir, "claude-session.json")
	writeJSON(t, sessionFile, sessionData)

	result := &launcher.Result{
		ExitCode:    0,
		Duration:    5 * time.Second,
		SessionPath: sessionFile,
	}

	err := aud.Capture("sess-001", "task-001", "owner/repo", 42, "dev-claude", result)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// Verify: session stored in SQLite.
	sessions, err := aud.Query(Filter{SessionID: "sess-001"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.SessionID != "sess-001" {
		t.Errorf("SessionID = %q, want %q", s.SessionID, "sess-001")
	}
	if s.TaskID != "task-001" {
		t.Errorf("TaskID = %q, want %q", s.TaskID, "task-001")
	}
	if s.IssueNum != 42 {
		t.Errorf("IssueNum = %d, want 42", s.IssueNum)
	}

	// Summary should contain tool call info.
	if !strings.Contains(s.Summary, "write_file") {
		t.Errorf("summary missing tool call 'write_file': %s", s.Summary)
	}
	if !strings.Contains(s.Summary, "Token Usage") {
		t.Errorf("summary missing token usage section: %s", s.Summary)
	}

	// Verify: raw file archived.
	if s.RawPath == "" {
		t.Fatal("RawPath is empty")
	}
	if _, err := os.Stat(s.RawPath); err != nil {
		t.Errorf("archived file not found: %v", err)
	}
}

// Scenario 2: Codex session capture
func TestCapture_Codex(t *testing.T) {
	aud, tmpDir := setup(t)

	logContent := `Starting codex execution...
Processing file: main.go
Warning: unused variable in line 10
Applying fix...
Completed successfully.
Result: 3 files modified.
`
	sessionFile := filepath.Join(tmpDir, "codex-log.txt")
	if err := os.WriteFile(sessionFile, []byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}

	result := &launcher.Result{
		ExitCode:    0,
		Duration:    3 * time.Second,
		SessionPath: sessionFile,
	}

	err := aud.Capture("sess-002", "task-002", "owner/repo", 7, "fix-codex", result)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	sessions, err := aud.Query(Filter{SessionID: "sess-002"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	s := sessions[0]

	// Should extract key lines from codex log.
	if !strings.Contains(s.Summary, "Codex Session Summary") {
		t.Errorf("summary missing codex header: %s", s.Summary)
	}
	if !strings.Contains(s.Summary, "Warning") {
		t.Errorf("summary missing key line 'Warning': %s", s.Summary)
	}
	if !strings.Contains(s.Summary, "Completed successfully") {
		t.Errorf("summary missing key line 'Completed': %s", s.Summary)
	}
}

func TestCapture_CodexEventSchemaArtifact(t *testing.T) {
	aud, tmpDir := setup(t)

	sessionFile := filepath.Join(tmpDir, "events-v1.jsonl")
	events := []launcherevents.Event{
		{
			Kind:      launcherevents.KindTurnStarted,
			Timestamp: time.Unix(1710000000, 0).UTC(),
			SessionID: "sess-events",
			TurnID:    "turn-1",
			Seq:       1,
			Payload:   mustJSON(t, launcherevents.TurnStartedPayload{TurnID: "turn-1"}),
		},
		{
			Kind:      launcherevents.KindCommandExec,
			Timestamp: time.Unix(1710000001, 0).UTC(),
			SessionID: "sess-events",
			TurnID:    "turn-1",
			Seq:       2,
			Payload:   mustJSON(t, launcherevents.CommandExecPayload{Cmd: []string{"bash", "-lc", "echo PONG"}, CallID: "cmd-1"}),
		},
		{
			Kind:      launcherevents.KindTokenUsage,
			Timestamp: time.Unix(1710000002, 0).UTC(),
			SessionID: "sess-events",
			TurnID:    "turn-1",
			Seq:       3,
			Payload:   mustJSON(t, launcherevents.TokenUsagePayload{Input: 10, Output: 4, Cached: 1, Total: 14}),
		},
	}
	var lines []string
	for _, evt := range events {
		data, err := json.Marshal(evt)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		lines = append(lines, string(data))
	}
	if err := os.WriteFile(sessionFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := &launcher.Result{
		ExitCode:    0,
		Duration:    2 * time.Second,
		LastMessage: "PONG",
		SessionPath: sessionFile,
	}

	if err := aud.Capture("sess-events", "task-events", "owner/repo", 8, "fix-codex", result); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	sessions, err := aud.Query(Filter{SessionID: "sess-events"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	summary := sessions[0].Summary
	if !strings.Contains(summary, "Event Counts") {
		t.Fatalf("summary missing event counts: %s", summary)
	}
	if !strings.Contains(summary, "bash -lc echo PONG") {
		t.Fatalf("summary missing command list: %s", summary)
	}
	if !strings.Contains(summary, "total: 14") {
		t.Fatalf("summary missing token usage: %s", summary)
	}
}

func TestRecordLabelValidation(t *testing.T) {
	aud, _ := setup(t)

	payload := LabelValidationPayload{
		Pre:            []string{"workbuddy", "status:developing"},
		Post:           []string{"workbuddy", "status:reviewing"},
		ExitCode:       0,
		Classification: "ok",
	}

	if err := aud.RecordLabelValidation("owner/repo", 9, payload); err != nil {
		t.Fatalf("RecordLabelValidation: %v", err)
	}

	events, err := aud.store.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != string(EventKindLabelValidation) {
		t.Fatalf("event type = %q, want %q", events[0].Type, EventKindLabelValidation)
	}

	var got LabelValidationPayload
	if err := json.Unmarshal([]byte(events[0].Payload), &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.Classification != payload.Classification {
		t.Fatalf("Classification = %q, want %q", got.Classification, payload.Classification)
	}
}

// Scenario 3: Large file truncation
func TestCapture_LargeFileTruncation(t *testing.T) {
	aud, tmpDir := setup(t)

	// Create a codex-style log where every line matches a keyword, producing
	// a summary that exceeds maxSummarySize (1 MB).
	var bigLog strings.Builder
	line := "error: " + strings.Repeat("X", 200) + "\n"
	for bigLog.Len() < maxSummarySize+10000 {
		bigLog.WriteString(line)
	}

	sessionFile := filepath.Join(tmpDir, "big-session.txt")
	if err := os.WriteFile(sessionFile, []byte(bigLog.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	result := &launcher.Result{
		ExitCode:    0,
		Duration:    1 * time.Second,
		SessionPath: sessionFile,
	}

	err := aud.Capture("sess-003", "task-003", "owner/repo", 99, "fix-codex", result)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	sessions, err := aud.Query(Filter{SessionID: "sess-003"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	s := sessions[0]

	// Summary must be truncated.
	if len(s.Summary) > maxSummarySize+200 { // allow some overhead for the truncation message
		t.Errorf("summary too large: %d bytes (max ~%d)", len(s.Summary), maxSummarySize)
	}
	if !strings.Contains(s.Summary, "[truncated, full session on disk]") {
		t.Error("summary missing truncation marker")
	}

	// Raw file on disk should be complete.
	rawData, err := os.ReadFile(s.RawPath)
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	originalData, _ := os.ReadFile(sessionFile)
	if len(rawData) != len(originalData) {
		t.Errorf("raw file size %d != original %d", len(rawData), len(originalData))
	}
}

// Scenario 4: Query by session_id, issue_num, agent_name
func TestQuery_Filters(t *testing.T) {
	aud, tmpDir := setup(t)

	// Insert several sessions via Capture (no session file — minimal records).
	entries := []struct {
		sessionID string
		taskID    string
		issueNum  int
		agentName string
	}{
		{"s1", "t1", 10, "dev-claude"},
		{"s2", "t2", 10, "review-claude"},
		{"s3", "t3", 20, "dev-claude"},
		{"s4", "t4", 20, "fix-codex"},
	}
	for _, e := range entries {
		result := &launcher.Result{ExitCode: 0, Duration: time.Second}
		// Create a dummy session file so we exercise the path.
		f := filepath.Join(tmpDir, e.sessionID+".txt")
		if err := os.WriteFile(f, []byte("log line\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		result.SessionPath = f
		if err := aud.Capture(e.sessionID, e.taskID, "owner/repo", e.issueNum, e.agentName, result); err != nil {
			t.Fatalf("Capture %s: %v", e.sessionID, err)
		}
	}

	// Query by session_id.
	res, err := aud.Query(Filter{SessionID: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].SessionID != "s2" {
		t.Errorf("session_id filter: got %d results", len(res))
	}

	// Query by issue_num.
	res, err = aud.Query(Filter{IssueNum: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Errorf("issue_num filter: expected 2, got %d", len(res))
	}

	// Query by agent_name.
	res, err = aud.Query(Filter{AgentName: "dev-claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Errorf("agent_name filter: expected 2, got %d", len(res))
	}

	// Combined filter.
	res, err = aud.Query(Filter{IssueNum: 20, AgentName: "fix-codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].SessionID != "s4" {
		t.Errorf("combined filter: expected s4, got %v", res)
	}
}

// Scenario: no session file — minimal summary
func TestCapture_NoSessionFile(t *testing.T) {
	aud, _ := setup(t)

	result := &launcher.Result{
		ExitCode: 1,
		Stdout:   "some output",
		Stderr:   "error occurred",
		Duration: 2 * time.Second,
	}

	err := aud.Capture("sess-none", "task-none", "owner/repo", 5, "dev-claude", result)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	sessions, err := aud.Query(Filter{SessionID: "sess-none"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1, got %d", len(sessions))
	}
	s := sessions[0]
	if !strings.Contains(s.Summary, "no session file") {
		t.Errorf("expected minimal summary, got: %s", s.Summary)
	}
	if s.RawPath != "" {
		t.Errorf("expected empty RawPath, got %q", s.RawPath)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
