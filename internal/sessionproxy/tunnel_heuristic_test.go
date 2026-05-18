package sessionproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// newReadCloser wraps a string in an io.ReadCloser for stubbed
// http.Response bodies. io.NopCloser doesn't accept strings directly.
func newReadCloser(body string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(body))
}

// #345 W4-A — regression tests pinning the sessionproxy fix that
// mirrors W3-B. The legacy WorkerRecord.Tunnel column is set once at
// registration and never updated when a worker reconnects after a
// network blip, so trusting it both misroutes traffic to workers that
// have just reconnected (column stuck at false) and keeps routing to
// workers whose row says true but whose tunnel has long since dropped.
//
// sessionproxy's three Tunnel reads — resolver.go:190 (single-session
// detail decision), handler.go:200 (the propagated res.Tunnel branch),
// and handler.go:588 (fan-out listing per-worker dispatch) — now gate on
// the in-memory wstunnel registry's live state via the new
// TunnelConnectivity interface. The wstunnel.Registry adds an endpoint
// when the WSS handshake completes and removes it on disconnect, so its
// state IS the truth.
//
// Each test below seeds the production failure case (column says
// Tunnel:false because the worker happened to register that way at
// boot, but Connected() reports true because the WSS tunnel is up) and
// asserts the routing decision; each is paired with the inverse guard
// (column says true but Connected() reports false because the tunnel
// dropped, so we must NOT route into dead infrastructure).

// fakeTunnels stubs both TunnelRoundTripper (Do) and TunnelConnectivity
// (Connected). Tests configure connectedSet to model which workers
// currently have a live WSS endpoint; Do records the dispatch so we
// can assert sessionproxy's transport decision.
type fakeTunnels struct {
	connectedMu  sync.RWMutex
	connectedSet map[string]bool

	hits    atomic.Int32
	mu      sync.RWMutex
	workers []string
}

func newFakeTunnels(connected ...string) *fakeTunnels {
	set := make(map[string]bool, len(connected))
	for _, id := range connected {
		set[id] = true
	}
	return &fakeTunnels{connectedSet: set}
}

func (f *fakeTunnels) Connected(workerID string) bool {
	f.connectedMu.RLock()
	defer f.connectedMu.RUnlock()
	return f.connectedSet[workerID]
}

func (f *fakeTunnels) Do(ctx context.Context, workerID string, req *http.Request) (*http.Response, error) {
	f.hits.Add(1)
	f.mu.Lock()
	f.workers = append(f.workers, workerID)
	f.mu.Unlock()
	// Return an empty JSON array body — tests only care about whether
	// the tunnel branch was taken, not the wire payload.
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       newReadCloser(`[]`),
		Request:    req,
	}, nil
}

func (f *fakeTunnels) Workers() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]string, len(f.workers))
	copy(out, f.workers)
	return out
}

// TestResolverRoutesViaTunnelWhenConnectedDespiteTunnelColumnFalse
// covers resolver.go:190 (the W3-B-style single-source-of-truth decision).
// Production scenario from #345: a worker reconnected after a network
// blip and re-established its WSS tunnel — Tunnels.Connected() now
// reports true — but WorkerRecord.Tunnel was set at the original
// registration and is still false. The resolver MUST route through the
// tunnel, not fall through to the (potentially unreachable) audit_url.
func TestResolverRoutesViaTunnelWhenConnectedDespiteTunnelColumnFalse(t *testing.T) {
	const (
		workerID  = "worker-blipped"
		sessionID = "sess-1"
	)
	dbPath := filepath.Join(t.TempDir(), "coord.db")
	st, err := store.NewCoordinatorStore(dbPath)
	if err != nil {
		t.Fatalf("NewCoordinatorStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// The lying column: Tunnel:false. Status:"online" + fresh
	// heartbeat (InsertWorker sets last_heartbeat = CURRENT_TIMESTAMP).
	if err := st.InsertWorker(store.WorkerRecord{
		ID:       workerID,
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://unreachable.worker.example:8091",
		Tunnel:   false,
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	if err := st.UpsertSessionRoute(store.SessionRoute{
		SessionID: sessionID,
		WorkerID:  workerID,
		Repo:      "org/repo",
		IssueNum:  42,
	}); err != nil {
		t.Fatalf("UpsertSessionRoute: %v", err)
	}

	tunnels := newFakeTunnels(workerID) // the live truth: connected
	res, err := NewResolver(st).WithTunnels(tunnels).Resolve(sessionID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res == nil {
		t.Fatal("Resolve returned nil")
	}
	if !res.Tunnel {
		t.Fatalf("Resolution.Tunnel = false, want true (Tunnels.Connected reports true; the legacy WorkerRecord.Tunnel column should not override that)")
	}
	if res.WorkerID != workerID {
		t.Fatalf("WorkerID = %q, want %q", res.WorkerID, workerID)
	}
}

// TestResolverDoesNotRouteViaTunnelWhenDisconnectedDespiteTunnelColumnTrue
// is the inverse guard. Row says Tunnel:true, Status:"online", fresh
// heartbeat — every legacy column-based signal would route via the
// tunnel — but the live wstunnel registry has no endpoint for this
// worker (the connection dropped and hasn't come back). The resolver
// MUST NOT route into dead infrastructure; it falls back to the
// audit_url branch, where a subsequent dial failure surfaces a clean
// worker_offline.
func TestResolverDoesNotRouteViaTunnelWhenDisconnectedDespiteTunnelColumnTrue(t *testing.T) {
	const (
		workerID  = "worker-dropped"
		sessionID = "sess-stale"
	)
	dbPath := filepath.Join(t.TempDir(), "coord.db")
	st, err := store.NewCoordinatorStore(dbPath)
	if err != nil {
		t.Fatalf("NewCoordinatorStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InsertWorker(store.WorkerRecord{
		ID:       workerID,
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://worker-dropped.example:8091",
		Tunnel:   true, // lies: actual WSS is down
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	if err := st.UpsertSessionRoute(store.SessionRoute{
		SessionID: sessionID,
		WorkerID:  workerID,
		Repo:      "org/repo",
		IssueNum:  1,
	}); err != nil {
		t.Fatalf("UpsertSessionRoute: %v", err)
	}

	tunnels := newFakeTunnels() // no live endpoints
	res, err := NewResolver(st).WithTunnels(tunnels).Resolve(sessionID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res == nil {
		t.Fatal("Resolve returned nil")
	}
	if res.Tunnel {
		t.Fatalf("Resolution.Tunnel = true, want false (the WSS endpoint is not live; routing here lands on dead infrastructure — the legacy WorkerRecord.Tunnel column must not short-circuit the live check)")
	}
	if res.AuditURL != "http://worker-dropped.example:8091" {
		t.Fatalf("AuditURL = %q, want fallback to the registered audit_url", res.AuditURL)
	}
}

// TestHandlerDetailRoutesViaTunnelWhenConnected covers handler.go:200
// (res.Tunnel propagation). Same production scenario, end-to-end: a
// single-session detail request for a worker that has Tunnel:false in
// the row but Connected:true in the registry must reach the
// TunnelRoundTripper, not h.client.
func TestHandlerDetailRoutesViaTunnelWhenConnected(t *testing.T) {
	const (
		workerID  = "worker-blipped"
		sessionID = "sess-detail"
	)
	dbPath := filepath.Join(t.TempDir(), "coord.db")
	st, err := store.NewCoordinatorStore(dbPath)
	if err != nil {
		t.Fatalf("NewCoordinatorStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InsertWorker(store.WorkerRecord{
		ID:       workerID,
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		// AuditURL set but unreachable — if the handler ignores the
		// tunnel decision and falls back to audit, the test will surface
		// it as worker_offline. The tunnels.hits assertion is the real
		// signal, not the response shape.
		AuditURL: "http://unreachable.audit.example:8091",
		Tunnel:   false,
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	if err := st.UpsertSessionRoute(store.SessionRoute{
		SessionID: sessionID,
		WorkerID:  workerID,
		Repo:      "org/repo",
		IssueNum:  7,
	}); err != nil {
		t.Fatalf("UpsertSessionRoute: %v", err)
	}

	tunnels := newFakeTunnels(workerID)
	h := NewHandler(HandlerConfig{
		Resolver: NewResolver(st).WithTunnels(tunnels),
		Tunnels:  tunnels,
		// Tight timeout so a wrong fallback to the audit branch surfaces
		// quickly as a test failure rather than hanging.
		HTTPClient: &http.Client{Timeout: 200 * time.Millisecond},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID, nil)
	h.ServeHTTP(rec, req)

	if got := tunnels.hits.Load(); got != 1 {
		t.Fatalf("tunnel.Do hits = %d, want 1 (detail request must route via the WSS tunnel because the registry reports Connected; the legacy WorkerRecord.Tunnel:false column must not gate it)", got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Workbuddy-Origin-Worker"); got != workerID {
		t.Fatalf("X-Workbuddy-Origin-Worker = %q, want %q", got, workerID)
	}
}

// TestHandlerDetailFallsBackWhenTunnelDropped: Tunnel:true in the row
// but the registry reports the WSS endpoint as gone. The detail handler
// must NOT call into the tunnel; it falls back to the audit_url branch
// and surfaces worker_offline when audit_url is unreachable.
func TestHandlerDetailFallsBackWhenTunnelDropped(t *testing.T) {
	const (
		workerID  = "worker-dropped"
		sessionID = "sess-stale-detail"
	)
	dbPath := filepath.Join(t.TempDir(), "coord.db")
	st, err := store.NewCoordinatorStore(dbPath)
	if err != nil {
		t.Fatalf("NewCoordinatorStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InsertWorker(store.WorkerRecord{
		ID:       workerID,
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://unreachable.worker.example:1",
		Tunnel:   true, // lies — actual WSS is down
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	if err := st.UpsertSessionRoute(store.SessionRoute{
		SessionID: sessionID,
		WorkerID:  workerID,
		Repo:      "org/repo",
		IssueNum:  9,
	}); err != nil {
		t.Fatalf("UpsertSessionRoute: %v", err)
	}

	tunnels := newFakeTunnels() // no live endpoints
	h := NewHandler(HandlerConfig{
		Resolver:   NewResolver(st).WithTunnels(tunnels),
		Tunnels:    tunnels,
		HTTPClient: &http.Client{Timeout: 200 * time.Millisecond},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID, nil)
	h.ServeHTTP(rec, req)

	if got := tunnels.hits.Load(); got != 0 {
		t.Fatalf("tunnel.Do hits = %d, want 0 (the WSS endpoint is not live; routing here lands on dead infrastructure)", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s, want 503 worker_offline from the audit_url fallback", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["degraded_reason"] != "worker_offline" {
		t.Fatalf("degraded_reason = %v, want worker_offline", body["degraded_reason"])
	}
}

// TestFanOutListingUsesTunnelWhenConnected covers handler.go:588. The
// fan-out path bypasses the resolver and reads worker.Tunnel directly
// per candidate. After the fix it gates on Connectivity.Connected:
// Tunnel:false but Connected:true must still dispatch via the tunnel.
func TestFanOutListingUsesTunnelWhenConnected(t *testing.T) {
	const workerID = "worker-blipped"
	dbPath := filepath.Join(t.TempDir(), "coord.db")
	st, err := store.NewCoordinatorStore(dbPath)
	if err != nil {
		t.Fatalf("NewCoordinatorStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InsertWorker(store.WorkerRecord{
		ID:       workerID,
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://unreachable.worker.example:8091",
		Tunnel:   false, // lies — the WSS reconnected after a blip
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	tunnels := newFakeTunnels(workerID)
	h := NewHandler(HandlerConfig{
		Resolver:         NewResolver(st).WithTunnels(tunnels),
		Tunnels:          tunnels,
		ListClient:       &http.Client{Timeout: 200 * time.Millisecond},
		PerWorkerTimeout: 200 * time.Millisecond,
		OverallTimeout:   500 * time.Millisecond,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?repo=org/repo", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := tunnels.hits.Load(); got != 1 {
		t.Fatalf("tunnel.Do hits = %d, want 1 (fan-out must dispatch via the WSS tunnel because the registry reports Connected)", got)
	}
	if workers := tunnels.Workers(); len(workers) != 1 || workers[0] != workerID {
		t.Fatalf("tunnel.Workers = %v, want [%s]", workers, workerID)
	}
	if offline := rec.Header().Get("X-Workbuddy-Worker-Offline"); offline != "" {
		t.Fatalf("X-Workbuddy-Worker-Offline = %q, want empty (the tunnel mock returns 200; pre-fix the audit_url branch would have dialled the unreachable URL and marked the worker offline)", offline)
	}
}

// TestFanOutListingSkipsTunnelWhenTunnelDropped: Tunnel:true in the row
// but the registry reports the WSS endpoint as gone. The fan-out must
// NOT dispatch via the tunnel; it falls through to the audit_url path
// and (here) the unreachable URL marks the worker offline — the safe
// failure mode.
func TestFanOutListingSkipsTunnelWhenTunnelDropped(t *testing.T) {
	const workerID = "worker-dropped"
	dbPath := filepath.Join(t.TempDir(), "coord.db")
	st, err := store.NewCoordinatorStore(dbPath)
	if err != nil {
		t.Fatalf("NewCoordinatorStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InsertWorker(store.WorkerRecord{
		ID:       workerID,
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://unreachable.worker.example:1",
		Tunnel:   true,
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	tunnels := newFakeTunnels() // no live endpoints
	h := NewHandler(HandlerConfig{
		Resolver:         NewResolver(st).WithTunnels(tunnels),
		Tunnels:          tunnels,
		ListClient:       &http.Client{Timeout: 200 * time.Millisecond},
		PerWorkerTimeout: 200 * time.Millisecond,
		OverallTimeout:   500 * time.Millisecond,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?repo=org/repo", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := tunnels.hits.Load(); got != 0 {
		t.Fatalf("tunnel.Do hits = %d, want 0 (the WSS endpoint is not live; routing here lands on dead infrastructure)", got)
	}
	if offline := rec.Header().Get("X-Workbuddy-Worker-Offline"); !strings.Contains(offline, workerID) {
		t.Fatalf("X-Workbuddy-Worker-Offline = %q, want it to include %q (audit_url branch should have been taken and dialled the unreachable URL)", offline, workerID)
	}
}
