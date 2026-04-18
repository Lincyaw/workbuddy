package workerclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientSendsBearerTokenOnEveryCall(t *testing.T) {
	var authCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization = %q", got)
		}
		authCalls.Add(1)
		switch r.URL.Path {
		case "/api/v1/workers/register":
			w.WriteHeader(http.StatusCreated)
		case "/api/v1/tasks/poll":
			_ = json.NewEncoder(w).Encode(Task{TaskID: "task-1", Repo: "owner/repo", IssueNum: 7, AgentName: "dev-agent"})
		case "/api/v1/tasks/task-1/heartbeat":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/tasks/task-1/result":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/tasks/task-1/release":
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := New(srv.URL, "secret-token", srv.Client())
	ctx := context.Background()
	if err := client.Register(ctx, RegisterRequest{WorkerID: "worker-1", Repo: "owner/repo", Roles: []string{"dev"}}); err != nil {
		t.Fatal(err)
	}
	task, err := client.PollTask(ctx, "worker-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.TaskID != "task-1" {
		t.Fatalf("unexpected task: %+v", task)
	}
	if err := client.Heartbeat(ctx, task.TaskID, HeartbeatRequest{WorkerID: "worker-1"}); err != nil {
		t.Fatal(err)
	}
	if err := client.SubmitResult(ctx, task.TaskID, ResultRequest{WorkerID: "worker-1", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseTask(ctx, task.TaskID, ReleaseRequest{WorkerID: "worker-1", Reason: "shutdown"}); err != nil {
		t.Fatal(err)
	}
	if got := authCalls.Load(); got != 5 {
		t.Fatalf("auth calls = %d, want 5", got)
	}
}

func TestClientPollTaskReturnsNilOnNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := New(srv.URL, "", srv.Client())
	task, err := client.PollTask(context.Background(), "worker-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if task != nil {
		t.Fatalf("expected nil task, got %+v", task)
	}
}

func TestClientReturnsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := New(srv.URL, "bad-token", srv.Client())
	err := client.Register(context.Background(), RegisterRequest{WorkerID: "worker-1", Repo: "owner/repo", Roles: []string{"dev"}})
	if err == nil {
		t.Fatal("expected unauthorized error")
	}
	if err != ErrUnauthorized {
		t.Fatalf("err = %v, want %v", err, ErrUnauthorized)
	}
}

func TestClientRetriesTransientFailures(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	client := New(srv.URL, "", srv.Client())
	client.maxBackoff = 10 * time.Millisecond
	if err := client.Register(context.Background(), RegisterRequest{WorkerID: "worker-1", Repo: "owner/repo", Roles: []string{"dev"}}); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestClientUnregister(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/api/v1/workers/worker-1" {
			t.Fatalf("path = %s, want /api/v1/workers/worker-1", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"unregistered"}`))
	}))
	defer srv.Close()

	client := New(srv.URL, "", srv.Client())
	if err := client.Unregister(context.Background(), "worker-1"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestClientUnregisterNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"worker not found"}`))
	}))
	defer srv.Close()

	client := New(srv.URL, "", srv.Client())
	err := client.Unregister(context.Background(), "missing-worker")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}
