package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/app"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestServeSessionDetailFallsBackToLocalForEmptyAuditURL is the
// end-to-end regression for must-fix #1: in `serve` mode the in-process
// worker registers without --audit-listen, so its `audit_url` column is
// empty. The sessionproxy resolver used to return ErrAuditURLMissing in
// that case, the handler emitted 503 "worker has no audit_url", and the
// SPA's session detail view broke.
//
// After the fix, an empty audit_url falls through to the coordinator's
// local handler. In serve mode the coordinator and worker share one
// SQLite DB (NewStore, NOT NewCoordinatorStore), so the local handler
// can read the row written by the in-process worker. The proxy must
// return 200 with the session JSON, not 503.
//
// We exercise the same buildCoordinatorMux that runServeWithOpts uses,
// seed a session row + worker row with empty audit_url, and assert via
// httptest that GET /api/v1/sessions/{id} returns 200.
func TestServeSessionDetailFallsBackToLocalForEmptyAuditURL(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "serve.db")
	// NewStore (not NewCoordinatorStore) is what runServeWithOpts uses
	// when sharedStoreWithWorker is true — the legacy session tables
	// remain because the in-process worker still writes to them.
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Worker registered with empty audit_url, mirroring serve mode where
	// the in-process worker doesn't pass --audit-listen.
	if err := st.InsertWorker(store.WorkerRecord{
		ID:       "in-process-worker",
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	const sessionID = "sess-serve-empty"
	if _, err := st.CreateSession(store.SessionRecord{
		SessionID: sessionID,
		Repo:      "org/repo",
		IssueNum:  7,
		AgentName: "dev-agent",
		WorkerID:  "in-process-worker",
		Status:    "running",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	api := &app.FullCoordinatorServer{Store: st, AuthEnabled: true, AuthToken: "serve-secret"}
	mux := buildCoordinatorMux(api, st, eventlog.NewEventLogger(st), dbPath, nil, nil, "")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	authedGet := func(path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("new request %s: %v", path, err)
		}
		req.Header.Set("Authorization", "Bearer serve-secret")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	resp := authedGet("/api/v1/sessions/" + sessionID)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (local fallback for empty audit_url) body=%s", resp.StatusCode, string(body))
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var detail map[string]any
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode: %v body=%s", err, string(body))
	}
	if got, _ := detail["session_id"].(string); got != sessionID {
		t.Fatalf("session_id = %v, want %s body=%s", detail["session_id"], sessionID, string(body))
	}
}
