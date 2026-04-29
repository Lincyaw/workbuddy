package webui

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// seedSession writes a session row + an events-v1.jsonl file so the events.json
// and stream paths have something to read.
func seedSession(t *testing.T, st *store.Store, sessionsDir, sessionID string) {
	t.Helper()
	dir := filepath.Join(sessionsDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "events-v1.jsonl"),
		[]byte("{\"kind\":\"a\",\"seq\":1}\n{\"kind\":\"b\",\"seq\":2}\n"),
		0o644,
	); err != nil {
		t.Fatalf("write events: %v", err)
	}
	if _, err := st.CreateSession(store.SessionRecord{
		SessionID: sessionID,
		Repo:      "owner/repo",
		IssueNum:  214,
		AgentName: "dev-agent",
		Status:    store.TaskStatusRunning,
		Dir:       dir,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
}

func TestDeprecatedEventsAndStreamPaths(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	seedSession(t, st, sessionsDir, "session-218")

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	h.Register(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	var logBuf bytes.Buffer
	originalOut := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(originalOut)
		log.SetFlags(originalFlags)
	})

	resp, err := http.Get(server.URL + "/sessions/session-218/events.json")
	if err != nil {
		t.Fatalf("GET legacy events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Deprecation"); got != "true" {
		t.Fatalf("Deprecation header = %q, want \"true\"", got)
	}
	if got := resp.Header.Get("Sunset"); got == "" {
		t.Fatalf("Sunset header missing")
	}
	if !strings.Contains(logBuf.String(), "[deprecated]") {
		t.Fatalf("expected [deprecated] log line, got %q", logBuf.String())
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), `"kind":"a"`) {
		t.Fatalf("body missing event content: %s", string(body))
	}
}

// TestAPISessionEventsParity ensures the new /api/v1/sessions/{id}/events path
// returns byte-for-byte the same JSON body as the legacy /sessions/{id}/
// events.json alias. This is the AC contract from #218: "diff 逐字节验".
func TestAPISessionEventsParity(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	seedSession(t, st, sessionsDir, "session-218")

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	h.Register(mux)
	mux.HandleFunc("/api/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		// Mirror the coordinator wiring: dispatch suffix to the right method.
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
		_, suffix, _ := strings.Cut(rest, "/")
		switch suffix {
		case "events":
			h.HandleAPISessionEvents(w, r)
		case "stream":
			h.HandleAPISessionStream(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	legacy, err := http.Get(server.URL + "/sessions/session-218/events.json?limit=50&offset=0")
	if err != nil {
		t.Fatalf("legacy GET: %v", err)
	}
	defer func() { _ = legacy.Body.Close() }()
	legacyBody, _ := io.ReadAll(legacy.Body)

	modern, err := http.Get(server.URL + "/api/v1/sessions/session-218/events?limit=50&offset=0")
	if err != nil {
		t.Fatalf("modern GET: %v", err)
	}
	defer func() { _ = modern.Body.Close() }()
	modernBody, _ := io.ReadAll(modern.Body)

	if !bytes.Equal(legacyBody, modernBody) {
		t.Fatalf("response bodies differ.\nlegacy=%s\nmodern=%s", string(legacyBody), string(modernBody))
	}
	// Modern path must NOT carry the deprecation header.
	if got := modern.Header.Get("Deprecation"); got != "" {
		t.Fatalf("modern path Deprecation = %q, want empty", got)
	}
}
