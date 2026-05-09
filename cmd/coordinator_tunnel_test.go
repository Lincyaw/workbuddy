package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/app"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/sessionproxy"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/wstunnel"
	"github.com/coder/websocket"
)

func TestCoordinatorTunnelSessionProxyEndToEnd(t *testing.T) {
	st, err := store.NewStore(t.TempDir() + "/coord.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	workerID := "worker-tunnel-1"
	if err := st.InsertWorker(store.WorkerRecord{ID: workerID, Repo: "owner/repo", ReposJSON: `["owner/repo"]`, Roles: `["role:dev"]`, Runtime: "codex", Hostname: "worker", Tunnel: true, Status: "online"}); err != nil {
		t.Fatalf("insert worker: %v", err)
	}
	tunnels := wstunnel.NewRegistry()
	api := &app.FullCoordinatorServer{Store: st, Registry: registry.NewRegistry(st, time.Second), Eventlog: eventlog.NewEventLogger(st), Tunnels: tunnels}
	proxy := sessionproxy.NewHandler(sessionproxy.HandlerConfig{Resolver: sessionproxy.NewResolver(st), Tunnels: tunnels, PerWorkerTimeout: 500 * time.Millisecond, OverallTimeout: time.Second})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workers/tunnel", api.HandleWorkerTunnel)
	mux.HandleFunc("/api/v1/sessions", proxy.ServeHTTP)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	block := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sessions":
			if r.URL.Query().Get("block") == "1" {
				<-block
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"session_id":"s-tunnel","repo":"owner/repo","created_at":"2026-05-09T00:00:00Z"}]`)
		default:
			http.NotFound(w, r)
		}
	})

	ep, cleanup := dialTestWorkerTunnel(t, srv.URL, workerID, handler)
	defer cleanup()

	resp, err := srv.Client().Get(srv.URL + "/api/v1/sessions")
	if err != nil {
		t.Fatalf("GET sessions: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sessions status=%d body=%s", resp.StatusCode, string(body))
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil || len(rows) != 1 || rows[0]["session_id"] != "s-tunnel" {
		t.Fatalf("sessions rows=%v err=%v body=%s", rows, err, string(body))
	}

	pending := make(chan int, 1)
	go func() {
		resp, err := srv.Client().Get(srv.URL + "/api/v1/sessions?block=1")
		if err != nil {
			pending <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		pending <- resp.StatusCode
	}()
	time.Sleep(50 * time.Millisecond)
	_ = ep.Close()
	select {
	case code := <-pending:
		if code != http.StatusOK && code != http.StatusBadGateway {
			t.Fatalf("pending status=%d, want 502 or degraded 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending request did not finish after tunnel close")
	}
	close(block)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		worker, err := st.GetWorker(workerID)
		if err != nil {
			t.Fatalf("get worker: %v", err)
		}
		if worker != nil && worker.Status == "offline" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("worker was not marked offline within 5s")
}

func dialTestWorkerTunnel(t *testing.T, baseURL, workerID string, handler http.Handler) (*wstunnel.Endpoint, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/api/v1/workers/tunnel?worker_id=" + workerID
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		cancel()
		t.Fatalf("dial tunnel: %v", err)
	}
	ep := wstunnel.NewEndpoint(conn)
	go ep.ServeRequests(context.Background(), handler)
	go ep.Run(context.Background())
	return ep, func() { _ = ep.Close(); cancel() }
}
