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

