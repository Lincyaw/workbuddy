package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/app"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestCoordinatorRoutesSessionsThroughSPAFallback verifies that /sessions and
// /sessions/{id} fall through to the SPA catch-all.
func TestCoordinatorRoutesSessionsThroughSPAFallback(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "coordinator.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed an events-v1.jsonl so the API paths have something to read.
	sessionsDir := filepath.Join(filepath.Dir(dbPath), "sessions")
	sessionDir := filepath.Join(sessionsDir, "abc-123")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(sessionDir, "events-v1.jsonl"),
		[]byte("{\"kind\":\"a\",\"seq\":1}\n"),
		0o644,
	); err != nil {
		t.Fatalf("write events: %v", err)
	}

	api := &app.FullCoordinatorServer{Store: st, AuthEnabled: true, AuthToken: "spa-secret"}
	mux := buildCoordinatorMux(api, st, eventlog.NewEventLogger(st), dbPath, nil, nil, "")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	authedGet := func(path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("new request %s: %v", path, err)
		}
		req.Header.Set("Authorization", "Bearer spa-secret")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	// /sessions falls to SPA.
	resp := authedGet("/sessions")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/sessions status = %d, want 200 (SPA)", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("/sessions Content-Type = %q, want text/html (SPA)", got)
	}
	_ = resp.Body.Close()

	// /sessions/abc-123 falls to SPA — no Deprecation header, html body.
	resp = authedGet("/sessions/abc-123")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/sessions/abc-123 status = %d, want 200 (SPA)", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("/sessions/abc-123 Content-Type = %q, want text/html (SPA)", got)
	}
	_ = resp.Body.Close()

	// /api/v1/sessions/abc-123/events hits the modern API.
	resp = authedGet("/api/v1/sessions/abc-123/events")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("events Content-Type = %q, want application/json", got)
	}
	_ = resp.Body.Close()
}
