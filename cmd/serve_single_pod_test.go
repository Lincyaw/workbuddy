package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/app"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestApplySinglePodWorkerGateDisablesTunnel proves ADR 2026-06-06 §5: in
// the `serve` topology the embedded worker must NOT open the reverse
// coordinator tunnel, because the coordinator runs in the same process and
// the tunnel would be a self-loop. The gate forces coordinatorTunnel off
// even when the field was (defensively) set true.
func TestApplySinglePodWorkerGateDisablesTunnel(t *testing.T) {
	opts := &workerOpts{singlePod: true, coordinatorTunnel: true}
	applySinglePodWorkerGate(opts)
	if opts.coordinatorTunnel {
		t.Fatalf("single-pod worker must not open the reverse coordinator tunnel (self-loop)")
	}
}

// TestApplySinglePodWorkerGateLeavesSplitHostUntouched guards the split-host
// path: a standalone worker (singlePod=false) keeps whatever tunnel setting
// it was configured with. Byte-for-byte unchanged behavior for the genuine
// coordinator+worker split deployment.
func TestApplySinglePodWorkerGateLeavesSplitHostUntouched(t *testing.T) {
	opts := &workerOpts{singlePod: false, coordinatorTunnel: true}
	applySinglePodWorkerGate(opts)
	if !opts.coordinatorTunnel {
		t.Fatalf("split-host worker tunnel setting must be left untouched")
	}
}

// TestSinglePodSessionReadsServeLocallyForLoopbackAuditURL is the §5
// regression for the coordinator side. In single-pod the resolver opts the
// loopback short-circuit IN (WithLocalAuditFallback), so a session whose
// owning worker advertises a *loopback* audit_url is served from the shared
// local store instead of the coordinator reverse-proxying to itself.
//
// We seed a worker with a 127.0.0.1 audit_url (no such server is actually
// listening) plus a session row, build the single-pod mux, and assert that
// both the listing and detail endpoints return 200 from the local handler.
// If the short-circuit were not enabled the coordinator would try to dial
// the (non-existent) loopback audit server and degrade to offline/503.
func TestSinglePodSessionReadsServeLocallyForLoopbackAuditURL(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "serve.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Worker advertises a loopback audit_url that points at a port where
	// nothing is listening. Split-host would dial it (and fail); single-pod
	// short-circuits to the local store.
	if err := st.InsertWorker(store.WorkerRecord{
		ID:       "in-process-worker",
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://127.0.0.1:1/",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	const sessionID = "sess-single-pod"
	if _, err := st.CreateSession(store.SessionRecord{
		SessionID: sessionID,
		Repo:      "org/repo",
		IssueNum:  9,
		AgentName: "dev-agent",
		WorkerID:  "in-process-worker",
		Status:    "running",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	api := &app.FullCoordinatorServer{Store: st, AuthEnabled: true, AuthToken: "serve-secret"}
	// singlePod=true is the gate under test.
	mux := buildCoordinatorMux(api, st, eventlog.NewEventLogger(st), dbPath, nil, nil, "", true)
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

	// Detail: must short-circuit to the local store, not dial the dead
	// loopback audit server.
	detailResp := authedGet("/api/v1/sessions/" + sessionID)
	defer func() { _ = detailResp.Body.Close() }()
	detailBody, _ := io.ReadAll(detailResp.Body)
	if detailResp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d, want 200 (local short-circuit) body=%s", detailResp.StatusCode, string(detailBody))
	}
	var detail map[string]any
	if err := json.Unmarshal(detailBody, &detail); err != nil {
		t.Fatalf("decode detail: %v body=%s", err, string(detailBody))
	}
	if got, _ := detail["session_id"].(string); got != sessionID {
		t.Fatalf("detail session_id = %v, want %s", detail["session_id"], sessionID)
	}

	// Listing: the fan-out must include the local row. The loopback worker's
	// dead audit server would otherwise surface it as offline.
	listResp := authedGet("/api/v1/sessions?repo=org/repo")
	defer func() { _ = listResp.Body.Close() }()
	listBody, _ := io.ReadAll(listResp.Body)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200 body=%s", listResp.StatusCode, string(listBody))
	}
	var rows []map[string]any
	if err := json.Unmarshal(listBody, &rows); err != nil {
		t.Fatalf("decode list: %v body=%s", err, string(listBody))
	}
	found := false
	for _, row := range rows {
		if id, _ := row["session_id"].(string); id == sessionID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("session %s not found in single-pod local listing: %s", sessionID, string(listBody))
	}
	if off := listResp.Header.Get("X-Workbuddy-Worker-Offline"); off != "" {
		t.Fatalf("single-pod listing should not mark any worker offline, got %q", off)
	}
}
