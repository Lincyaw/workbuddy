package coordinator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/workerclient"
)

func newTestServerStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "workbuddy.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestTaskEndpointsRequireBearerToken(t *testing.T) {
	st := newTestServerStore(t)
	srv := httptest.NewServer(NewServer(st, ServerOptions{}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/tasks/poll?worker_id=worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestRevokedTokenReturnsUnauthorized(t *testing.T) {
	st := newTestServerStore(t)
	issued, err := st.IssueWorkerToken("worker-1", "owner/repo", []string{"dev"}, "host-1")
	if err != nil {
		t.Fatalf("IssueWorkerToken: %v", err)
	}
	if err := st.RevokeWorkerToken("worker-1", issued.KID); err != nil {
		t.Fatalf("RevokeWorkerToken: %v", err)
	}

	srv := httptest.NewServer(NewServer(st, ServerOptions{}))
	defer srv.Close()

	client := workerclient.New(srv.URL, issued.Token, srv.Client())
	_, err = client.PollTask(context.Background(), "worker-1", 0)
	if err == nil {
		t.Fatal("expected PollTask to fail with revoked token")
	}
	if err != workerclient.ErrUnauthorized {
		t.Fatalf("err = %v, want %v", err, workerclient.ErrUnauthorized)
	}
}

func TestLoopbackModeBypassesAuth(t *testing.T) {
	st := newTestServerStore(t)
	srv := httptest.NewServer(NewServer(st, ServerOptions{LoopbackOnly: true}))
	defer srv.Close()

	client := workerclient.New(srv.URL, "", srv.Client())
	task, err := client.PollTask(context.Background(), "worker-1", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("PollTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil task (no content), got %+v", task)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	st := newTestServerStore(t)
	srv := httptest.NewServer(NewServer(st, ServerOptions{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; version=0.0.4" {
		t.Fatalf("content type = %q, want %q", got, "text/plain; version=0.0.4")
	}
}
