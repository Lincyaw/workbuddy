package audit

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
	st, err := store.NewStore(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedSession inserts a task + session record + on-disk artefacts that
// mirror what runtime/session_manager produces in production.
func seedSession(t *testing.T, st *store.Store, sessionsDir, sessionID, status, eventsBody string) {
	t.Helper()
	dir := filepath.Join(sessionsDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte("stdout"), 0o644); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stderr"), []byte("stderr"), 0o644); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	if eventsBody != "" {
		if err := os.WriteFile(filepath.Join(dir, "events-v1.jsonl"), []byte(eventsBody), 0o644); err != nil {
			t.Fatalf("write events: %v", err)
		}
	}

	taskID := "task-" + sessionID
	if err := st.InsertTask(store.TaskRecord{
		ID:        taskID,
		Repo:      "owner/repo",
		IssueNum:  101,
		AgentName: "dev-agent",
		Status:    status,
		WorkerID:  "worker-A",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := st.CreateSession(store.SessionRecord{
		SessionID:  sessionID,
		TaskID:     taskID,
		Repo:       "owner/repo",
		IssueNum:   101,
		AgentName:  "dev-agent",
		Runtime:    "claude-code",
		WorkerID:   "worker-A",
		Attempt:    1,
		Status:     status,
		Dir:        dir,
		StdoutPath: filepath.Join(dir, "stdout"),
		StderrPath: filepath.Join(dir, "stderr"),
		Summary:    "summary " + sessionID,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func newAuditFixture(t *testing.T, token string) (*httptest.Server, *store.Store, string) {
	t.Helper()
	st := newTestStore(t)
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	mux := buildMux(Config{
		Listen:      "127.0.0.1:0",
		Token:       token,
		Store:       st,
		SessionsDir: sessionsDir,
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, st, sessionsDir
}

func mustGet(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestAuditServer_RejectsMissingToken(t *testing.T) {
	server, _, _ := newAuditFixture(t, "secret")

	resp := mustGet(t, server.URL+"/api/v1/sessions", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuditServer_RejectsWrongToken(t *testing.T) {
	server, _, _ := newAuditFixture(t, "secret")

	resp := mustGet(t, server.URL+"/api/v1/sessions", "wrong-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuditServer_HealthOpen(t *testing.T) {
	server, _, _ := newAuditFixture(t, "secret")

	resp := mustGet(t, server.URL+"/health", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAuditServer_ListSessions(t *testing.T) {
	server, st, sessionsDir := newAuditFixture(t, "secret")
	seedSession(t, st, sessionsDir, "sess-completed", store.TaskStatusCompleted, "{\"kind\":\"log\"}\n")

	resp := mustGet(t, server.URL+"/api/v1/sessions", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("session count = %d, want 1: %+v", len(body), body)
	}
	if body[0]["session_id"] != "sess-completed" {
		t.Fatalf("session_id = %v, want sess-completed", body[0]["session_id"])
	}
}

func TestAuditServer_SessionDetail(t *testing.T) {
	server, st, sessionsDir := newAuditFixture(t, "secret")
	seedSession(t, st, sessionsDir, "sess-detail", store.TaskStatusCompleted, "{\"kind\":\"log\"}\n")

	resp := mustGet(t, server.URL+"/api/v1/sessions/sess-detail", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["session_id"] != "sess-detail" {
		t.Fatalf("session_id = %v", body["session_id"])
	}
	if body["status"] != store.TaskStatusCompleted {
		t.Fatalf("status = %v, want %s", body["status"], store.TaskStatusCompleted)
	}
	// degraded must be false (events file present + DB row present)
	if got, _ := body["degraded"].(bool); got {
		t.Fatalf("degraded = true, want false (events file exists): %+v", body)
	}
}

// TestAuditServer_NoDiskOnlySynthesis verifies the worker-side handler
// does NOT synthesize a fake "aborted_before_start" detail when only
// disk metadata exists (this is the whole point of Phase 1: the worker's
// DB is authoritative for session existence).
func TestAuditServer_NoDiskOnlySynthesis(t *testing.T) {
	server, _, sessionsDir := newAuditFixture(t, "secret")

	// Plant a metadata.json on disk with no DB row.
	dir := filepath.Join(sessionsDir, "ghost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	meta := map[string]any{
		"session_id": "ghost",
		"repo":       "owner/repo",
		"issue_num":  1,
		"agent_name": "dev-agent",
		"status":     "completed",
	}
	body, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), body, 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	resp := mustGet(t, server.URL+"/api/v1/sessions/ghost", "secret")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (worker handler must not synthesize from disk)", resp.StatusCode)
	}
}

func TestAuditServer_DegradedNoEventsFile(t *testing.T) {
	server, st, sessionsDir := newAuditFixture(t, "secret")
	// Completed session with no events-v1.jsonl on disk should still
	// be flagged degraded:no_events_file.
	seedSession(t, st, sessionsDir, "sess-no-events", store.TaskStatusCompleted, "")

	resp := mustGet(t, server.URL+"/api/v1/sessions/sess-no-events", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := body["degraded"].(bool); !got {
		t.Fatalf("degraded = false, want true (no events file)")
	}
	if got, _ := body["degraded_reason"].(string); got != "no_events_file" {
		t.Fatalf("degraded_reason = %q, want no_events_file", got)
	}
}

func TestAuditServer_SessionEvents(t *testing.T) {
	server, st, sessionsDir := newAuditFixture(t, "secret")
	seedSession(t, st, sessionsDir, "sess-events", store.TaskStatusCompleted,
		`{"v":1,"ts":"2026-05-01T00:00:00Z","kind":"log","data":{"msg":"hello"}}`+"\n")

	resp := mustGet(t, server.URL+"/api/v1/sessions/sess-events/events", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	events, ok := body["events"].([]any)
	if !ok {
		t.Fatalf("events not an array: %+v", body)
	}
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
}

func TestAuditServer_StreamRejectsMissingToken(t *testing.T) {
	server, st, sessionsDir := newAuditFixture(t, "secret")
	seedSession(t, st, sessionsDir, "sess-stream", store.TaskStatusRunning,
		`{"v":1,"ts":"2026-05-01T00:00:00Z","kind":"log"}`+"\n")

	resp := mustGet(t, server.URL+"/api/v1/sessions/sess-stream/stream", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestStartRefusesEmptyToken(t *testing.T) {
	st := newTestStore(t)
	_, err := Start(Config{
		Listen:      "127.0.0.1:0",
		Token:       "",
		Store:       st,
		SessionsDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error when token is empty")
	}
	if !strings.Contains(err.Error(), "token is required") {
		t.Fatalf("error = %v, want token-required", err)
	}
}

func TestStartHappyPath(t *testing.T) {
	st := newTestStore(t)
	srv, err := Start(Config{
		Listen:      "127.0.0.1:0",
		Token:       "secret",
		Store:       st,
		SessionsDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if srv.Addr() == "" {
		t.Fatal("Addr() empty after Start")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	// Hit the health endpoint to confirm the server is serving.
	resp, err := http.Get("http://" + srv.Addr() + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health status = %d", resp.StatusCode)
	}
}

func TestAdvertisedURL(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		public   string
		hostname string
		want     string
	}{
		{
			name: "public override wins",
			addr: "127.0.0.1:8091", public: "https://worker.example.com/audit",
			hostname: "ignored", want: "https://worker.example.com/audit",
		},
		{
			name: "loopback bind",
			addr: "127.0.0.1:8091", hostname: "host", want: "http://127.0.0.1:8091",
		},
		{
			name: "wildcard bind uses hostname",
			addr: "0.0.0.0:8091", hostname: "worker-a", want: "http://worker-a:8091",
		},
		{
			name: "wildcard bind without hostname falls back to loopback",
			addr: "0.0.0.0:8091", hostname: "", want: "http://127.0.0.1:8091",
		},
		{
			name: "ipv6 wildcard",
			addr: "[::]:8091", hostname: "worker-b", want: "http://worker-b:8091",
		},
		{
			name: "malformed addr returns empty",
			addr: "not-an-addr", hostname: "worker", want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AdvertisedURL(tc.addr, tc.public, tc.hostname)
			if got != tc.want {
				t.Fatalf("AdvertisedURL(%q,%q,%q) = %q, want %q",
					tc.addr, tc.public, tc.hostname, got, tc.want)
			}
		})
	}
}
