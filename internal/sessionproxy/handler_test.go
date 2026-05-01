package sessionproxy

import (
	"context"
	"encoding/json"
	"fmt"
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

// fakeWorker is a minimal stand-in for the worker-side audit server.
// It records request hits + presented Authorization, and answers with
// canned bodies. Each test wires up the routes it needs.
type fakeWorker struct {
	t        *testing.T
	mu       sync.Mutex
	hits     int
	authSeen string
	srv      *httptest.Server
	handler  http.Handler
}

func newFakeWorker(t *testing.T, h http.Handler) *fakeWorker {
	t.Helper()
	fw := &fakeWorker{t: t}
	fw.handler = h
	fw.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fw.mu.Lock()
		fw.hits++
		fw.authSeen = r.Header.Get("Authorization")
		fw.mu.Unlock()
		fw.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(fw.srv.Close)
	return fw
}

func (fw *fakeWorker) URL() string { return fw.srv.URL }

func (fw *fakeWorker) Hits() int {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return fw.hits
}

func (fw *fakeWorker) Auth() string {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return fw.authSeen
}

// newTestStore returns a fresh sqlite-backed store with a session →
// worker mapping, mimicking what the coordinator would have after a
// dispatch + register cycle.
func newTestStore(t *testing.T, sessionID, workerID, auditURL string) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if workerID != "" {
		if err := st.InsertWorker(store.WorkerRecord{
			ID:       workerID,
			Repo:     "org/repo",
			Roles:    `["dev"]`,
			Hostname: "host",
			AuditURL: auditURL,
			Status:   "online",
		}); err != nil {
			t.Fatalf("InsertWorker: %v", err)
		}
	}
	if sessionID != "" {
		if _, err := st.CreateSession(store.SessionRecord{
			SessionID: sessionID,
			Repo:      "org/repo",
			IssueNum:  1,
			AgentName: "dev-agent",
			WorkerID:  workerID,
			Status:    "running",
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	return st
}

func TestProxySingleSessionHappyPath(t *testing.T) {
	const (
		sessionID = "sess-123"
		workerID  = "worker-a"
		token     = "secret-token"
	)
	worker := newFakeWorker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/v1/sessions/"+sessionID {
			t.Errorf("path = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"sess-123","status":"running"}`))
	}))
	st := newTestStore(t, sessionID, workerID, worker.URL())

	h := NewHandler(HandlerConfig{
		Resolver:  NewResolver(st),
		AuthToken: token,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID, nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"session_id":"sess-123"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if worker.Hits() != 1 {
		t.Fatalf("worker hits = %d", worker.Hits())
	}
}

func TestProxyForwardsBearerToken(t *testing.T) {
	const token = "secret-token"
	worker := newFakeWorker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_id":"sess-123"}`))
	}))
	st := newTestStore(t, "sess-123", "worker-a", worker.URL())
	h := NewHandler(HandlerConfig{Resolver: NewResolver(st), AuthToken: token})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-123", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := worker.Auth(); got != "Bearer "+token {
		t.Fatalf("worker auth = %q, want Bearer %s", got, token)
	}
}

func TestProxyWorkerOffline(t *testing.T) {
	// Stand up + close a fake worker to get a known-bad URL.
	worker := newFakeWorker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := worker.URL()
	worker.srv.Close() // intentional: closed listener returns dial errors

	st := newTestStore(t, "sess-123", "worker-a", url)
	h := NewHandler(HandlerConfig{
		Resolver:  NewResolver(st),
		AuthToken: "tok",
		HTTPClient: &http.Client{Timeout: 500 * time.Millisecond},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-123", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if body["degraded_reason"] != "worker_offline" {
		t.Fatalf("degraded_reason = %v", body["degraded_reason"])
	}
	if body["worker_id"] != "worker-a" {
		t.Fatalf("worker_id = %v", body["worker_id"])
	}
}

func TestProxySSEStreamForwardsEventsAndCursors(t *testing.T) {
	// Worker streams 3 events with explicit ids; proxy must preserve
	// each frame and flush so the client sees them in real time.
	worker := newFakeWorker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for i := 1; i <= 3; i++ {
			_, _ = fmt.Fprintf(w, "id: %d\nevent: sample\ndata: {\"n\":%d}\n\n", i, i)
			fl.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	st := newTestStore(t, "sess-123", "worker-a", worker.URL())
	h := NewHandler(HandlerConfig{Resolver: NewResolver(st), AuthToken: "tok"})

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/sessions/sess-123/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content-type = %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for i := 1; i <= 3; i++ {
		marker := fmt.Sprintf("id: %d", i)
		if !strings.Contains(string(body), marker) {
			t.Fatalf("missing %q in body: %s", marker, body)
		}
	}
}

func TestProxySSEBrowserDisconnectPropagates(t *testing.T) {
	upstreamCancelled := make(chan struct{}, 1)
	worker := newFakeWorker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		<-r.Context().Done()
		upstreamCancelled <- struct{}{}
	}))
	st := newTestStore(t, "sess-123", "worker-a", worker.URL())
	h := NewHandler(HandlerConfig{Resolver: NewResolver(st), AuthToken: "tok"})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/sessions/sess-123/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
		_ = resp.Body.Close()
	}()
	_, _ = io.ReadAll(resp.Body)

	select {
	case <-upstreamCancelled:
	case <-time.After(2 * time.Second):
		t.Fatalf("upstream did not observe cancellation")
	}
}

// fanoutSetup builds a coordinator-side proxy with two workers, each
// returning a small canned listing.
type fanoutWorker struct {
	id   string
	rows []map[string]any
	w    *fakeWorker
}

func startFanoutWorker(t *testing.T, id string, rows []map[string]any) *fanoutWorker {
	w := newFakeWorker(t, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(rows)
	}))
	return &fanoutWorker{id: id, rows: rows, w: w}
}

func TestFanoutListingHappyPath(t *testing.T) {
	now := time.Now().UTC()
	a := startFanoutWorker(t, "worker-a", []map[string]any{
		{"session_id": "s-1", "created_at": now.Format(time.RFC3339), "agent_name": "dev"},
		{"session_id": "s-2", "created_at": now.Add(-1 * time.Minute).Format(time.RFC3339), "agent_name": "dev"},
	})
	b := startFanoutWorker(t, "worker-b", []map[string]any{
		{"session_id": "s-3", "created_at": now.Add(-30 * time.Second).Format(time.RFC3339), "agent_name": "review"},
		{"session_id": "s-4", "created_at": now.Add(-2 * time.Minute).Format(time.RFC3339), "agent_name": "review"},
	})

	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-a", Repo: "org/repo", AuditURL: a.w.URL(), Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-b", Repo: "org/other", AuditURL: b.w.URL(), Status: "online"}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(HandlerConfig{Resolver: NewResolver(st), AuthToken: "tok"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d body=%s", len(rows), rec.Body.String())
	}
	// Sort: created_at desc → s-1, s-3, s-2, s-4.
	want := []string{"s-1", "s-3", "s-2", "s-4"}
	for i, sid := range want {
		got, _ := rows[i]["session_id"].(string)
		if got != sid {
			t.Fatalf("rows[%d].session_id = %s, want %s", i, got, sid)
		}
	}
	if got := rec.Header().Get("X-Workbuddy-Worker-Offline"); got != "" {
		t.Fatalf("worker_offline header = %q (want empty)", got)
	}
}

func TestFanoutListingOneWorkerOffline(t *testing.T) {
	now := time.Now().UTC()
	a := startFanoutWorker(t, "worker-a", []map[string]any{
		{"session_id": "s-1", "created_at": now.Format(time.RFC3339)},
	})
	// b: closed listener
	b := newFakeWorker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	bURL := b.URL()
	b.srv.Close()

	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-a", Repo: "org/repo", AuditURL: a.w.URL(), Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-b", Repo: "org/repo", AuditURL: bURL, Status: "online"}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(HandlerConfig{
		Resolver:  NewResolver(st),
		AuthToken: "tok",
		ListClient: &http.Client{Timeout: 300 * time.Millisecond},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var rows []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0]["session_id"] != "s-1" {
		t.Fatalf("rows = %+v", rows)
	}
	if got := rec.Header().Get("X-Workbuddy-Worker-Offline"); got != "worker-b" {
		t.Fatalf("worker_offline = %q", got)
	}
}

func TestFanoutCacheHit(t *testing.T) {
	now := time.Now().UTC()
	a := startFanoutWorker(t, "worker-a", []map[string]any{
		{"session_id": "s-1", "created_at": now.Format(time.RFC3339)},
	})
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-a", Repo: "org/repo", AuditURL: a.w.URL(), Status: "online"}); err != nil {
		t.Fatal(err)
	}
	var dispatchCount int32
	h := NewHandler(HandlerConfig{Resolver: NewResolver(st), AuthToken: "tok"})
	h.dispatchHook = func(string) { atomic.AddInt32(&dispatchCount, 1) }

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d status = %d", i, rec.Code)
		}
	}
	// Only the first call should have hit the worker.
	if got := atomic.LoadInt32(&dispatchCount); got != 1 {
		t.Fatalf("dispatch count = %d, want 1", got)
	}
}

// TestFanoutCacheInvalidationOnWorkerRegister verifies the Phase 3
// invalidation hook: after the first fan-out call caches the listing,
// adding a new worker and calling InvalidateCache makes the next
// listing visit BOTH workers, not just the cached set.
func TestFanoutCacheInvalidationOnWorkerRegister(t *testing.T) {
	now := time.Now().UTC()
	a := startFanoutWorker(t, "worker-a", []map[string]any{
		{"session_id": "s-1", "created_at": now.Format(time.RFC3339)},
	})
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-a", Repo: "org/repo", AuditURL: a.w.URL(), Status: "online"}); err != nil {
		t.Fatal(err)
	}

	dispatched := make(map[string]int)
	var dispatchMu sync.Mutex
	h := NewHandler(HandlerConfig{Resolver: NewResolver(st), AuthToken: "tok"})
	h.dispatchHook = func(workerID string) {
		dispatchMu.Lock()
		defer dispatchMu.Unlock()
		dispatched[workerID]++
	}

	// First call: only worker-a is registered.
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("call 1 status = %d", rec1.Code)
	}

	// Second call: still cached → no new dispatch.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("call 2 status = %d", rec2.Code)
	}
	if rec2.Header().Get("X-Workbuddy-Cache") != "hit" {
		t.Fatalf("call 2 should be a cache hit, got header %q", rec2.Header().Get("X-Workbuddy-Cache"))
	}

	// Add worker-b and invalidate.
	b := startFanoutWorker(t, "worker-b", []map[string]any{
		{"session_id": "s-2", "created_at": now.Format(time.RFC3339)},
	})
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-b", Repo: "org/repo", AuditURL: b.w.URL(), Status: "online"}); err != nil {
		t.Fatal(err)
	}
	h.InvalidateCache()

	// Third call: cache cleared, should hit BOTH workers and surface s-2.
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("call 3 status = %d", rec3.Code)
	}
	if rec3.Header().Get("X-Workbuddy-Cache") == "hit" {
		t.Fatalf("call 3 should NOT be a cache hit after InvalidateCache")
	}
	dispatchMu.Lock()
	defer dispatchMu.Unlock()
	if got := dispatched["worker-b"]; got != 1 {
		t.Fatalf("worker-b dispatched %d times, want 1 (was the new worker visible after invalidation?)", got)
	}
	if !strings.Contains(rec3.Body.String(), `"s-2"`) {
		t.Fatalf("call 3 body missing new worker's session: %s", rec3.Body.String())
	}
}

func TestProxyLocalLoopbackShortCircuits(t *testing.T) {
	// audit_url points at the coordinator's loopback. Resolver should
	// flag Local=true and the proxy should hand the request to the
	// LocalHandler instead of dialling.
	called := false
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`"local"`))
	})
	st := newTestStore(t, "sess-123", "worker-a", "http://127.0.0.1:65535")
	h := NewHandler(HandlerConfig{
		Resolver:  NewResolver(st).WithLocalAuditFallback(true),
		Local:     local,
		AuthToken: "tok",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-123", nil)
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatalf("local handler not called; body=%s", rec.Body.String())
	}
}

// TestProxyEmptyAuditURLFallsBackToLocal exercises the must-fix #1
// behaviour: a worker that registered with an empty audit_url (typical
// in `serve` mode where the in-process worker doesn't pass
// --audit-listen, or rolling a new coordinator out before its workers)
// should fall through to the coordinator's local handler instead of
// 503-ing with "worker has no audit_url".
func TestProxyEmptyAuditURLFallsBackToLocal(t *testing.T) {
	called := false
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"sess-empty","status":"running"}`))
	})
	st := newTestStore(t, "sess-empty", "worker-empty", "")
	h := NewHandler(HandlerConfig{
		Resolver:  NewResolver(st),
		Local:     local,
		AuthToken: "tok",
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-empty", nil)
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatalf("local handler not called for empty audit_url; status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"sess-empty"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestFanoutEmptyAuditURLIncludesLocalListing exercises the must-fix #1
// fan-out behaviour: when ANY candidate worker has an empty audit_url
// AND a local handler is configured, the fan-out makes one call into
// the local handler so the coordinator-local store contributes rows
// instead of those workers being silently dropped as "offline".
func TestFanoutEmptyAuditURLIncludesLocalListing(t *testing.T) {
	now := time.Now().UTC()
	a := startFanoutWorker(t, "worker-a", []map[string]any{
		{"session_id": "remote-1", "created_at": now.Format(time.RFC3339)},
	})
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-a", Repo: "org/repo", AuditURL: a.w.URL(), Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-empty", Repo: "org/repo", AuditURL: "", Status: "online"}); err != nil {
		t.Fatal(err)
	}

	localHits := 0
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"session_id":"local-1","created_at":"` + now.Add(-15*time.Second).Format(time.RFC3339) + `"}]`))
	})

	h := NewHandler(HandlerConfig{
		Resolver:  NewResolver(st),
		Local:     local,
		AuthToken: "tok",
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d body=%s", len(rows), rec.Body.String())
	}
	got := []string{rows[0]["session_id"].(string), rows[1]["session_id"].(string)}
	if got[0] != "remote-1" || got[1] != "local-1" {
		t.Fatalf("rows order = %v, want [remote-1 local-1]", got)
	}
	if localHits != 1 {
		t.Fatalf("local handler hit %d times, want 1 (one shared call covers all empty-audit_url workers)", localHits)
	}
	if hdr := rec.Header().Get("X-Workbuddy-Worker-Offline"); hdr != "" {
		t.Fatalf("X-Workbuddy-Worker-Offline = %q, want empty (empty-audit_url workers should not be marked offline when local fallback succeeds)", hdr)
	}
}

// TestFanoutEmptyAuditURLNoLocalHandlerMarksOffline verifies the
// degraded path: when no local handler is configured, an empty-audit_url
// worker is reported as offline (so the SPA can render the banner).
func TestFanoutEmptyAuditURLNoLocalHandlerMarksOffline(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-empty", Repo: "org/repo", AuditURL: "", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(HandlerConfig{Resolver: NewResolver(st), AuthToken: "tok"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Workbuddy-Worker-Offline"); got != "worker-empty" {
		t.Fatalf("worker_offline = %q, want worker-empty", got)
	}
}

