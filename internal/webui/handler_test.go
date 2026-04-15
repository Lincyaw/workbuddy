package webui

import (
	"context"
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
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedSessions(t *testing.T, st *store.Store) {
	t.Helper()
	sessions := []store.AgentSession{
		{SessionID: "session-aaa", TaskID: "task-1", Repo: "org/repo", IssueNum: 1, AgentName: "dev-agent", Summary: "Fixed the bug"},
		{SessionID: "session-bbb", TaskID: "task-2", Repo: "org/repo", IssueNum: 2, AgentName: "test-agent", Summary: "Ran tests"},
		{SessionID: "session-ccc", TaskID: "task-3", Repo: "org/other", IssueNum: 1, AgentName: "dev-agent", Summary: "Added feature"},
	}
	for _, s := range sessions {
		if _, err := st.InsertAgentSession(s); err != nil {
			t.Fatalf("InsertAgentSession: %v", err)
		}
	}
}

func TestHandleList_Empty(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No sessions found") {
		t.Fatalf("expected empty state message, got: %s", w.Body.String())
	}
}

func TestHandleList_WithSessions(t *testing.T) {
	st := newTestStore(t)
	seedSessions(t, st)
	h := NewHandler(st)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "session-aaa") {
		t.Error("expected session-aaa in list")
	}
	if !strings.Contains(body, "session-bbb") {
		t.Error("expected session-bbb in list")
	}
	if !strings.Contains(body, "session-ccc") {
		t.Error("expected session-ccc in list")
	}
}

func TestHandleList_FilterByRepo(t *testing.T) {
	st := newTestStore(t)
	seedSessions(t, st)
	h := NewHandler(st)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions?repo=org/other", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "session-ccc") {
		t.Error("expected session-ccc in filtered list")
	}
	if strings.Contains(body, "session-aaa") {
		t.Error("session-aaa should not appear in filtered list")
	}
}

func TestHandleList_FilterByIssue(t *testing.T) {
	st := newTestStore(t)
	seedSessions(t, st)
	h := NewHandler(st)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions?issue=2", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "session-bbb") {
		t.Error("expected session-bbb in filtered list")
	}
	if strings.Contains(body, "session-aaa") {
		t.Error("session-aaa should not appear when filtering by issue 2")
	}
}

func TestHandleList_FilterByAgent(t *testing.T) {
	st := newTestStore(t)
	seedSessions(t, st)
	h := NewHandler(st)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions?agent=test-agent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "session-bbb") {
		t.Error("expected session-bbb for test-agent filter")
	}
	if strings.Contains(body, "session-aaa") {
		t.Error("session-aaa should not appear for test-agent filter")
	}
}

func TestHandleList_ShowsTaskStatus(t *testing.T) {
	st := newTestStore(t)
	seedSessions(t, st)
	// Insert matching tasks so LEFT JOIN yields statuses.
	if err := st.InsertTask(store.TaskRecord{ID: "task-1", Repo: "org/repo", IssueNum: 1, AgentName: "dev-agent", Status: store.TaskStatusRunning}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if err := st.InsertTask(store.TaskRecord{ID: "task-2", Repo: "org/repo", IssueNum: 2, AgentName: "test-agent", Status: store.TaskStatusFailed}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	h := NewHandler(st)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "status-running") {
		t.Error("expected running status badge")
	}
	if !strings.Contains(body, "status-failed") {
		t.Error("expected failed status badge")
	}
}

func TestHandleDetail_Found(t *testing.T) {
	st := newTestStore(t)
	seedSessions(t, st)
	h := NewHandler(st)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions/session-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "session-aaa") {
		t.Error("expected session ID in detail page")
	}
	if !strings.Contains(body, "dev-agent") {
		t.Error("expected agent name in detail page")
	}
	if !strings.Contains(body, "Fixed the bug") {
		t.Error("expected summary in detail page")
	}
}

func TestHandleDetail_JSON(t *testing.T) {
	st := newTestStore(t)
	seedSessions(t, st)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)

	if err := os.MkdirAll(filepath.Join(dir, "session-aaa"), 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-aaa", "events-v1.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write events file: %v", err)
	}

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions/session-aaa?format=json", nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["session_id"] != "session-aaa" {
		t.Fatalf("session_id = %#v", body["session_id"])
	}
}

func TestHandleDetail_NotFound(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Session Not Found") {
		t.Error("expected not found message")
	}
}

func TestHandleDetail_EmptyID_Redirects(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/sessions" {
		t.Fatalf("expected redirect to /sessions, got %s", loc)
	}
}

func TestSessionURL(t *testing.T) {
	url := SessionURL("session-abc")
	if url != "/sessions/session-abc" {
		t.Fatalf("expected /sessions/session-abc, got %s", url)
	}
}

// writeEventsFile lays out <dir>/<sessionID>/events-v1.jsonl with the given lines.
func writeEventsFile(t *testing.T, dir, sessionID string, lines []string) {
	t.Helper()
	sessionDir := filepath.Join(dir, sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "events-v1.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
}

func sampleEventLine(seq uint64, kind, text string) string {
	payload, _ := json.Marshal(map[string]any{"text": text})
	evt := map[string]any{
		"kind":       kind,
		"ts":         "2026-04-15T04:00:00Z",
		"session_id": "session-t",
		"turn_id":    "turn-t",
		"seq":        seq,
		"payload":    json.RawMessage(payload),
	}
	b, _ := json.Marshal(evt)
	return string(b)
}

func TestEventsJSON_OffsetLimit(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)

	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, sampleEventLine(uint64(i+1), "log", "line"))
	}
	writeEventsFile(t, dir, "session-t", lines)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions/session-t/events.json?offset=3&limit=4", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp eventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	if resp.Total != 10 {
		t.Fatalf("total = %d, want 10", resp.Total)
	}
	if resp.Start != 3 || resp.End != 7 {
		t.Fatalf("start/end = %d/%d, want 3/7", resp.Start, resp.End)
	}
	if len(resp.Events) != 4 {
		t.Fatalf("events = %d, want 4", len(resp.Events))
	}
	if resp.Events[0].Index != 3 {
		t.Fatalf("first index = %d, want 3", resp.Events[0].Index)
	}
}

func TestEventsJSON_Tail(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)

	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, sampleEventLine(uint64(i+1), "log", "line"))
	}
	writeEventsFile(t, dir, "session-t", lines)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions/session-t/events.json?tail=1&limit=3", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp eventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 10 || resp.Start != 7 || resp.End != 10 {
		t.Fatalf("total/start/end = %d/%d/%d, want 10/7/10", resp.Total, resp.Start, resp.End)
	}
	if len(resp.Events) != 3 || resp.Events[0].Index != 7 {
		t.Fatalf("events = %+v", resp.Events)
	}
}

func TestEventsJSON_PayloadTruncation(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)

	// One event with a text field larger than maxStringLen.
	big := strings.Repeat("x", maxStringLen+500)
	line := sampleEventLine(1, "log", big)
	writeEventsFile(t, dir, "session-t", []string{line})

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions/session-t/events.json", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp eventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d", len(resp.Events))
	}
	if !resp.Events[0].Truncated {
		t.Fatalf("expected truncated flag on oversized payload")
	}
	payload, ok := resp.Events[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T", resp.Events[0].Payload)
	}
	text, _ := payload["text"].(string)
	if len(text) == 0 || len(text) > maxStringLen+100 {
		t.Fatalf("text length = %d, expected <= ~maxStringLen", len(text))
	}
}

func TestEventsJSON_RejectsPathTraversal(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)

	// Pre-create a file outside the sessions dir that a naive join could reach.
	secret := filepath.Join(filepath.Dir(dir), "secret-events-v1.jsonl")
	if err := os.WriteFile(secret, []byte(`{"kind":"secret"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	for _, sid := range []string{"..", ".", "../secret", "foo/bar", "foo\\bar"} {
		req := httptest.NewRequest("GET", "/sessions/"+sid+"/events.json", nil)
		w := httptest.NewRecorder()
		// Route directly to the subpath dispatcher.
		h.handleSessionSubpath(w, req)
		if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("sessionID %q leaked secret content: %s", sid, w.Body.String())
		}
	}
}

func TestStream_SmokeEmitsEvent(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)
	writeEventsFile(t, dir, "session-t", []string{sampleEventLine(1, "log", "hello")})

	mux := http.NewServeMux()
	h.Register(mux)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/sessions/session-t/stream", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	go func() {
		// Give the handler just enough time to drain the initial file and
		// write the first event before the request context expires.
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "event: evt") {
		t.Fatalf("expected SSE event line, got: %s", body)
	}
	if !strings.Contains(body, `"kind":"log"`) {
		t.Fatalf("expected event payload in body, got: %s", body)
	}
}
