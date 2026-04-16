package auditapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedAuditData(t *testing.T, st *store.Store, sessionsDir string) {
	t.Helper()
	if _, err := st.InsertEvent(store.Event{
		Type:     "dispatch",
		Repo:     "owner/repo",
		IssueNum: 40,
		Payload:  `{"agent":"dev-agent"}`,
	}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 40,
		Labels:   `["status:reviewing","type:feature"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if _, err := st.IncrementTransition("owner/repo", 40, "developing", "reviewing"); err != nil {
		t.Fatalf("IncrementTransition 1: %v", err)
	}
	if _, err := st.IncrementTransition("owner/repo", 40, "reviewing", "developing"); err != nil {
		t.Fatalf("IncrementTransition 2: %v", err)
	}
	if err := st.UpsertIssueDependencyState(store.IssueDependencyState{
		Repo:              "owner/repo",
		IssueNum:          40,
		Verdict:           store.DependencyVerdictBlocked,
		ResumeLabel:       "status:developing",
		BlockedReasonHash: "abc123",
		GraphVersion:      7,
	}); err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}
	rawPath := filepath.Join(sessionsDir, "session-40", "codex-exec.jsonl")
	if err := os.MkdirAll(filepath.Dir(rawPath), 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "session-40", "events-v1.jsonl"), []byte("{\"kind\":\"log\"}\n"), 0o644); err != nil {
		t.Fatalf("write events-v1: %v", err)
	}
	if err := os.WriteFile(rawPath, []byte("{\"type\":\"task_started\"}\n"), 0o644); err != nil {
		t.Fatalf("write raw session: %v", err)
	}
	if _, err := st.InsertAgentSession(store.AgentSession{
		SessionID: "session-40",
		TaskID:    "task-40",
		Repo:      "owner/repo",
		IssueNum:  40,
		AgentName: "dev-agent",
		Summary:   "summary",
		RawPath:   rawPath,
	}); err != nil {
		t.Fatalf("InsertAgentSession: %v", err)
	}
}

func TestHandleEvents(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)
	seedAuditData(t, st, dir)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/events?repo=owner/repo&issue=40&type=dispatch&since=2020-01-01T00:00:00Z", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp eventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(resp.Events))
	}
	if resp.Events[0].Type != "dispatch" {
		t.Fatalf("type = %q", resp.Events[0].Type)
	}
	payload, ok := resp.Events[0].Payload.(map[string]any)
	if !ok || payload["agent"] != "dev-agent" {
		t.Fatalf("payload = %#v", resp.Events[0].Payload)
	}
}

func TestHandleIssueState(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)
	seedAuditData(t, st, dir)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/issues/owner/repo/40/state", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp issueStateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CycleCount != 2 {
		t.Fatalf("cycle_count = %d, want 2", resp.CycleCount)
	}
	if resp.DependencyVerdict != store.DependencyVerdictBlocked {
		t.Fatalf("dependency_verdict = %q", resp.DependencyVerdict)
	}
	if len(resp.Labels) != 2 || resp.Labels[0] != "status:reviewing" {
		t.Fatalf("labels = %#v", resp.Labels)
	}
}

func TestHandleSession(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)
	seedAuditData(t, st, dir)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions/session-40", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp SessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SessionID != "session-40" {
		t.Fatalf("session_id = %q", resp.SessionID)
	}
	if !strings.HasSuffix(resp.ArtifactPaths.EventsV1, filepath.Join("session-40", "events-v1.jsonl")) {
		t.Fatalf("events path = %q", resp.ArtifactPaths.EventsV1)
	}
	if !strings.HasSuffix(resp.ArtifactPaths.Raw, filepath.Join("session-40", "codex-exec.jsonl")) {
		t.Fatalf("raw path = %q", resp.ArtifactPaths.Raw)
	}
}

func TestHandleEvents_InvalidSince(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/events?since=not-a-time", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestParseIssueStatePath(t *testing.T) {
	repo, issueNum, ok := parseIssueStatePath("/issues/owner/repo/40/state")
	if !ok {
		t.Fatal("expected valid path")
	}
	if repo != "owner/repo" || issueNum != 40 {
		t.Fatalf("repo/issue = %q/%d", repo, issueNum)
	}
}

func TestDecodeJSONOrString(t *testing.T) {
	got := decodeJSONOrString(`{"ok":true}`)
	m, ok := got.(map[string]any)
	if !ok || m["ok"] != true {
		t.Fatalf("decoded = %#v", got)
	}
	if decodeJSONOrString("") != nil {
		t.Fatal("empty payload should decode to nil")
	}
	if s, ok := decodeJSONOrString("not-json").(string); !ok || s != "not-json" {
		t.Fatalf("fallback = %#v", decodeJSONOrString("not-json"))
	}
}

func TestSessionResponseJSONTime(t *testing.T) {
	resp := SessionResponse{CreatedAt: time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "2026-04-15T10:00:00Z") {
		t.Fatalf("json = %s", string(data))
	}
}
