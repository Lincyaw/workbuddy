package webui

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
