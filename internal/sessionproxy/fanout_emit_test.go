// REQ-158 / #345 Wave 6: regression + instrumentation tests for the
// fan-out emitter contract.
//
// Pre-fix, /api/v1/sessions returned an empty body with
// X-Workbuddy-Worker-Offline:<id> and no log line on the coordinator
// when the WSS-tunnel-routed fan-out call hit a 401 at the worker mgmt
// mux (the tunnel path forgot to forward Authorization, even though the
// worker mgmt mux protects /api/v1/sessions with the same bearer the
// direct audit-URL path already forwarded). These tests pin:
//
//   1. Each of the four fan-out outcomes emits exactly one structured
//      event with the correct type and worker_id payload.
//   2. The bearer-forward fix: a tunnel-dispatched listing request now
//      carries the coordinator's AuthToken on the request headers so
//      the worker mgmt mux's wrapBearerAuth lets it through.
package sessionproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// recordedEvent captures a single h.emit invocation. Tests assert on
// the type and a small subset of the payload so they remain robust to
// future payload additions (e.g. adding latency_ms).
type recordedEvent struct {
	eventType string
	repo      string
	issueNum  int
	payload   map[string]any
}

// recordingEmitter is a thread-safe sink for emit calls. The fan-out
// fires goroutines per candidate worker; each may emit concurrently.
type recordingEmitter struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (re *recordingEmitter) Emit(eventType, repo string, issueNum int, payload interface{}) {
	rec := recordedEvent{eventType: eventType, repo: repo, issueNum: issueNum}
	// Round-trip through JSON so map[string]any payloads decode the
	// same way they would after eventlog persists them — matches the
	// shape ops actually queries.
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err == nil {
			_ = json.Unmarshal(raw, &rec.payload)
		}
	}
	re.mu.Lock()
	re.events = append(re.events, rec)
	re.mu.Unlock()
}

func (re *recordingEmitter) snapshot() []recordedEvent {
	re.mu.Lock()
	defer re.mu.Unlock()
	out := make([]recordedEvent, len(re.events))
	copy(out, re.events)
	return out
}

// errorTunnels is a TunnelRoundTripper + TunnelConnectivity stub that
// always errors on Do — models the production "tunnel up but worker
// mgmt mux returned 401 → ErrWorkerOffline" case.
type errorTunnels struct {
	connected map[string]bool
	err       error
	// gotAuth records the Authorization header presented on the most
	// recent Do call. Used by the bearer-forward regression test.
	mu      sync.Mutex
	gotAuth string
}

func (e *errorTunnels) Connected(workerID string) bool { return e.connected[workerID] }

func (e *errorTunnels) Do(_ context.Context, _ string, req *http.Request) (*http.Response, error) {
	e.mu.Lock()
	e.gotAuth = req.Header.Get("Authorization")
	e.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	// Default: behave like fakeTunnels.Do — return a 200 with [].
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       newReadCloser(`[]`),
		Request:    req,
	}, nil
}

func (e *errorTunnels) lastAuth() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gotAuth
}

// seedWorkerStore inserts a single worker with the given fields and
// returns the store. Used to keep the per-test setup brief.
func seedWorkerStore(t *testing.T, workerID, repo, auditURL string) store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertWorker(store.WorkerRecord{
		ID: workerID, Repo: repo, AuditURL: auditURL, Status: "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	return st
}

// findEventByWorker locates the single event for workerID, failing the
// test when zero or multiple matches exist (the spec guarantees one
// emit per fan-out attempt per worker).
func findEventByWorker(t *testing.T, events []recordedEvent, workerID string) recordedEvent {
	t.Helper()
	var matches []recordedEvent
	for _, ev := range events {
		if got, _ := ev.payload["worker_id"].(string); got == workerID {
			matches = append(matches, ev)
		}
	}
	if len(matches) == 0 {
		t.Fatalf("no fan-out event found for worker %q; saw %d events: %+v", workerID, len(events), events)
	}
	if len(matches) > 1 {
		t.Fatalf("expected one fan-out event for worker %q, got %d: %+v", workerID, len(matches), matches)
	}
	return matches[0]
}

// TestFanoutEmitTunnelOK covers the Connected=true + tunnel returns 200
// happy path. The handler must emit exactly one
// sessionproxy_fanout_tunnel_ok event with the worker_id and row count.
func TestFanoutEmitTunnelOK(t *testing.T) {
	const (
		workerID = "worker-tunnel-ok"
		repo     = "org/repo"
	)
	st := seedWorkerStore(t, workerID, repo, "")
	tunnels := &errorTunnels{connected: map[string]bool{workerID: true}}
	rec := &recordingEmitter{}

	h := NewHandler(HandlerConfig{
		Resolver:     NewResolver(st),
		AuthToken:    "tok-xyz",
		Tunnels:      tunnels,
		Connectivity: tunnels,
		EventEmit:    rec.Emit,
	})
	_, _, _, err := h.fanOutListing(context.Background(), map[string][]string{"repo": {repo}})
	if err != nil {
		t.Fatalf("fanOutListing: %v", err)
	}

	events := rec.snapshot()
	ev := findEventByWorker(t, events, workerID)
	if ev.eventType != eventlogTypeSessionproxyFanoutTunnelOK {
		t.Fatalf("event type = %q, want %q", ev.eventType, eventlogTypeSessionproxyFanoutTunnelOK)
	}
	if got, _ := ev.payload["repo"].(string); got != repo {
		t.Fatalf("payload.repo = %q, want %q", got, repo)
	}
	if _, ok := ev.payload["rows"]; !ok {
		t.Fatalf("payload missing rows field: %+v", ev.payload)
	}
}

// TestFanoutEmitTunnelError covers the Connected=true + tunnel errors
// path. This is the silent failure mode from production: pre-fix the
// coordinator just appended worker_id to X-Workbuddy-Worker-Offline
// with no log line. The handler must emit
// sessionproxy_fanout_tunnel_error so ops can see WHY.
func TestFanoutEmitTunnelError(t *testing.T) {
	const (
		workerID = "worker-tunnel-err"
		repo     = "org/repo"
	)
	st := seedWorkerStore(t, workerID, repo, "")
	tunnels := &errorTunnels{
		connected: map[string]bool{workerID: true},
		err:       errors.New("tunnel timeout"),
	}
	rec := &recordingEmitter{}

	h := NewHandler(HandlerConfig{
		Resolver:     NewResolver(st),
		AuthToken:    "tok-xyz",
		Tunnels:      tunnels,
		Connectivity: tunnels,
		EventEmit:    rec.Emit,
	})
	_, offline, _, err := h.fanOutListing(context.Background(), map[string][]string{"repo": {repo}})
	if err != nil {
		t.Fatalf("fanOutListing: %v", err)
	}
	if len(offline) != 1 || offline[0] != workerID {
		t.Fatalf("offline = %v, want [%s]", offline, workerID)
	}

	events := rec.snapshot()
	ev := findEventByWorker(t, events, workerID)
	if ev.eventType != eventlogTypeSessionproxyFanoutTunnelError {
		t.Fatalf("event type = %q, want %q", ev.eventType, eventlogTypeSessionproxyFanoutTunnelError)
	}
	if got, _ := ev.payload["error"].(string); got == "" {
		t.Fatalf("payload.error = empty; want non-empty wrapping the tunnel err")
	}
}

// TestFanoutEmitAuditOK covers the Connected=false + AuditURL set +
// worker returns 200 path. Emits sessionproxy_fanout_audit_ok.
func TestFanoutEmitAuditOK(t *testing.T) {
	const (
		workerID = "worker-audit-ok"
		repo     = "org/repo"
	)
	now := time.Now().UTC()
	worker := startFanoutWorker(t, workerID, []map[string]any{
		{"session_id": "s-1", "created_at": now.Format(time.RFC3339)},
	})
	st := seedWorkerStore(t, workerID, repo, worker.w.URL())
	// Connected: false ⇒ fan-out picks the audit-URL branch.
	tunnels := &errorTunnels{connected: map[string]bool{}}
	rec := &recordingEmitter{}

	h := NewHandler(HandlerConfig{
		Resolver:     NewResolver(st),
		AuthToken:    "tok-xyz",
		Tunnels:      tunnels,
		Connectivity: tunnels,
		EventEmit:    rec.Emit,
	})
	_, _, _, err := h.fanOutListing(context.Background(), map[string][]string{"repo": {repo}})
	if err != nil {
		t.Fatalf("fanOutListing: %v", err)
	}

	events := rec.snapshot()
	ev := findEventByWorker(t, events, workerID)
	if ev.eventType != eventlogTypeSessionproxyFanoutAuditOK {
		t.Fatalf("event type = %q, want %q", ev.eventType, eventlogTypeSessionproxyFanoutAuditOK)
	}
	if got, _ := ev.payload["audit_url"].(string); got == "" {
		t.Fatalf("payload.audit_url empty; want the advertised URL")
	}
}

// TestFanoutEmitOfflineNoAudit covers the Connected=false +
// AuditURL=empty path. The handler must emit
// sessionproxy_fanout_offline_no_audit so ops can distinguish "the
// worker advertised no audit_url" (config gap on the worker host) from
// "the worker advertised an audit_url that's unreachable" (network gap).
func TestFanoutEmitOfflineNoAudit(t *testing.T) {
	const (
		workerID = "worker-no-audit"
		repo     = "org/repo"
	)
	st := seedWorkerStore(t, workerID, repo, "")
	tunnels := &errorTunnels{connected: map[string]bool{}}
	rec := &recordingEmitter{}

	h := NewHandler(HandlerConfig{
		Resolver:     NewResolver(st),
		AuthToken:    "tok-xyz",
		Tunnels:      tunnels,
		Connectivity: tunnels,
		EventEmit:    rec.Emit,
		// No Local handler — the empty-audit_url worker degrades to
		// offline rather than serving via in-process fallback. This
		// matches the split-host production topology this Wave is
		// instrumenting.
	})
	_, _, _, err := h.fanOutListing(context.Background(), map[string][]string{"repo": {repo}})
	if err != nil {
		t.Fatalf("fanOutListing: %v", err)
	}

	events := rec.snapshot()
	ev := findEventByWorker(t, events, workerID)
	if ev.eventType != eventlogTypeSessionproxyFanoutOfflineNoAudit {
		t.Fatalf("event type = %q, want %q", ev.eventType, eventlogTypeSessionproxyFanoutOfflineNoAudit)
	}
}

// TestFanoutTunnelListingForwardsBearer pins the actual bug fix: the
// tunnel-dispatched listing call MUST present the coordinator's bearer
// token so the worker mgmt mux's wrapBearerAuth lets it through. Pre-
// fix this header was unset, the mgmt mux returned 401, and the
// coordinator silently appended the worker to
// X-Workbuddy-Worker-Offline with no log line.
//
// Regression scenario: production worker `ddq-MS-7E17` heart-beating
// fine and tunnel up, yet GET /api/v1/sessions?repo=Lincyaw/AgentM kept
// returning [] with X-Workbuddy-Worker-Offline:ddq-MS-7E17.
func TestFanoutTunnelListingForwardsBearer(t *testing.T) {
	const (
		workerID = "worker-tunnel-auth"
		repo     = "org/repo"
		token    = "super-secret-token"
	)
	st := seedWorkerStore(t, workerID, repo, "")
	tunnels := &errorTunnels{connected: map[string]bool{workerID: true}}

	h := NewHandler(HandlerConfig{
		Resolver:     NewResolver(st),
		AuthToken:    token,
		Tunnels:      tunnels,
		Connectivity: tunnels,
	})
	_, _, _, err := h.fanOutListing(context.Background(), map[string][]string{"repo": {repo}})
	if err != nil {
		t.Fatalf("fanOutListing: %v", err)
	}
	if got, want := tunnels.lastAuth(), "Bearer "+token; got != want {
		t.Fatalf("tunnel Do() Authorization header = %q, want %q (the tunnel listing path must forward the coordinator bearer to the worker mgmt mux; the direct audit-URL path already did)", got, want)
	}
}
