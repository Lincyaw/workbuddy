package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

func TestClientRun_TaskLifecycleAgainstFakeCoordinator(t *testing.T) {
	t.Parallel()

	task := Task{
		ID:       "task-1",
		Repo:     "owner/repo",
		IssueNum: 46,
		Agent: config.AgentConfig{
			Name:    "dev-agent",
			Runtime: "fake-runtime",
			Prompt:  "fix issue",
		},
		Context: launcher.TaskContext{
			Repo:     "owner/repo",
			RepoRoot: t.TempDir(),
			WorkDir:  t.TempDir(),
			Session:  launcher.SessionContext{ID: "session-task-1"},
		},
		Workflow: "dev-workflow",
		State:    "developing",
	}

	coord := newFakeCoordinator(task)
	server := httptest.NewServer(coord.handler())
	defer server.Close()

	lnch := launcher.NewLauncher()
	runtime := &fakeRuntime{
		delay: 60 * time.Millisecond,
		result: &launcher.Result{
			ExitCode: 0,
			Stdout:   "task completed",
			Duration: 60 * time.Millisecond,
			Meta:     map[string]string{"pr_url": "https://example.test/pr/46"},
			SessionRef: launcher.SessionRef{
				ID:   "session-task-1",
				Kind: "fake",
			},
			TokenUsage: &launcherevents.TokenUsagePayload{Input: 12, Output: 34, Total: 46},
		},
	}
	lnch.Register(runtime, "fake-runtime")

	client := newTestClient(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Run(ctx, LauncherExecutor{Launcher: lnch})
	}()

	result := coord.waitForResult(t, 3*time.Second)
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context canceled", err)
	}

	if coord.registers != 1 {
		t.Fatalf("registers = %d, want 1", coord.registers)
	}
	if !coord.acked["task-1"] {
		t.Fatal("task was not acked")
	}
	if got := coord.heartbeats["task-1"]; got < 1 {
		t.Fatalf("heartbeats = %d, want >= 1", got)
	}
	if result.Result.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", result.Result.ExitCode)
	}
	if result.Result.Stdout != "task completed" {
		t.Fatalf("stdout = %q, want task completed", result.Result.Stdout)
	}
	if result.Result.Meta["pr_url"] != "https://example.test/pr/46" {
		t.Fatalf("pr_url = %q", result.Result.Meta["pr_url"])
	}
	if runtime.executedTask == nil || runtime.executedTask.Context.Session.ID != "session-task-1" {
		t.Fatalf("launcher did not receive expected task context: %+v", runtime.executedTask)
	}
}

func TestClientRun_ReconnectsAfterCoordinatorRestart(t *testing.T) {
	t.Parallel()

	task := Task{
		ID:       "task-restart",
		Repo:     "owner/repo",
		IssueNum: 47,
		Agent: config.AgentConfig{
			Name:    "review-agent",
			Runtime: "fake-runtime",
			Prompt:  "review issue",
		},
		Context: launcher.TaskContext{
			Repo:     "owner/repo",
			RepoRoot: t.TempDir(),
			WorkDir:  t.TempDir(),
			Session:  launcher.SessionContext{ID: "session-restart"},
		},
	}

	var (
		mu              sync.Mutex
		firstRegisters  int
		secondRegisters int
		pollStarted     = make(chan struct{})
		resultDone      = make(chan SubmitResultRequest, 1)
	)

	firstHandler := http.NewServeMux()
	firstHandler.HandleFunc("/api/v1/workers/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		firstRegisters++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	firstHandler.HandleFunc("/api/v1/tasks/poll", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-pollStarted:
		default:
			close(pollStarted)
		}
		<-r.Context().Done()
	})

	addr, shutdownFirst := startHTTPServer(t, firstHandler)

	lnch := launcher.NewLauncher()
	lnch.Register(&fakeRuntime{
		delay: 20 * time.Millisecond,
		result: &launcher.Result{
			ExitCode: 0,
			Stdout:   "resumed",
			Duration: 20 * time.Millisecond,
		},
	}, "fake-runtime")

	client, err := New(Config{
		BaseURL:           "http://" + addr,
		WorkerID:          "worker-1",
		Repo:              "owner/repo",
		Roles:             []string{"dev"},
		HeartbeatInterval: 10 * time.Millisecond,
		BackoffInitial:    10 * time.Millisecond,
		BackoffMax:        40 * time.Millisecond,
		HTTPClient:        &http.Client{Timeout: 200 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Run(ctx, LauncherExecutor{Launcher: lnch})
	}()

	select {
	case <-pollStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for long poll to start")
	}
	shutdownFirst()

	secondHandler := http.NewServeMux()
	secondHandler.HandleFunc("/api/v1/workers/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		secondRegisters++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	secondHandler.HandleFunc("/api/v1/tasks/poll", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(PollResult{Task: &task})
	})
	secondHandler.HandleFunc("/api/v1/tasks/task-restart/ack", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	secondHandler.HandleFunc("/api/v1/tasks/task-restart/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	secondHandler.HandleFunc("/api/v1/tasks/task-restart/result", func(w http.ResponseWriter, r *http.Request) {
		var req SubmitResultRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case resultDone <- req:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	_, shutdownSecond := startHTTPServerOnAddr(t, addr, secondHandler)
	defer shutdownSecond()

	select {
	case submitted := <-resultDone:
		if submitted.Result.Stdout != "resumed" {
			t.Fatalf("stdout = %q, want resumed", submitted.Result.Stdout)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for worker to resume after restart")
	}

	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context canceled", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if firstRegisters < 1 {
		t.Fatalf("first register count = %d, want >= 1", firstRegisters)
	}
	if secondRegisters < 1 {
		t.Fatalf("second register count = %d, want >= 1", secondRegisters)
	}
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()

	client, err := New(Config{
		BaseURL:           baseURL,
		WorkerID:          "worker-1",
		Repo:              "owner/repo",
		Roles:             []string{"dev"},
		Hostname:          "worker-host",
		HeartbeatInterval: 10 * time.Millisecond,
		BackoffInitial:    10 * time.Millisecond,
		BackoffMax:        40 * time.Millisecond,
		HTTPClient:        &http.Client{Timeout: 500 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

type fakeCoordinator struct {
	mu         sync.Mutex
	task       Task
	registers  int
	polled     int
	acked      map[string]bool
	heartbeats map[string]int
	resultCh   chan SubmitResultRequest
}

func newFakeCoordinator(task Task) *fakeCoordinator {
	return &fakeCoordinator{
		task:       task,
		acked:      make(map[string]bool),
		heartbeats: make(map[string]int),
		resultCh:   make(chan SubmitResultRequest, 1),
	}
}

func (c *fakeCoordinator) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workers/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var req RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.registers++
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/tasks/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		timeout := parseTimeout(r.URL.Query().Get("timeout"))
		deadline := time.NewTimer(timeout)
		defer deadline.Stop()

		c.mu.Lock()
		c.polled++
		shouldSend := c.polled == 1
		task := c.task
		c.mu.Unlock()

		if shouldSend {
			_ = json.NewEncoder(w).Encode(PollResult{Task: &task})
			return
		}

		select {
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/api/v1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
		parts := strings.Split(trimmed, "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		taskID, action := parts[0], parts[1]

		switch action {
		case "ack":
			c.mu.Lock()
			c.acked[taskID] = true
			c.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "heartbeat":
			c.mu.Lock()
			c.heartbeats[taskID]++
			c.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "result":
			var req SubmitResultRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			select {
			case c.resultCh <- req:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func (c *fakeCoordinator) waitForResult(t *testing.T, timeout time.Duration) SubmitResultRequest {
	t.Helper()
	select {
	case result := <-c.resultCh:
		return result
	case <-time.After(timeout):
		t.Fatal("timed out waiting for result submission")
		return SubmitResultRequest{}
	}
}

func parseTimeout(raw string) time.Duration {
	if raw == "" {
		return 100 * time.Millisecond
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 100 * time.Millisecond
	}
	return time.Duration(seconds * float64(time.Second))
}

type fakeRuntime struct {
	delay        time.Duration
	result       *launcher.Result
	err          error
	mu           sync.Mutex
	executedTask *Task
}

func (r *fakeRuntime) Name() string { return "fake-runtime" }

func (r *fakeRuntime) Start(_ context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (launcher.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.executedTask = &Task{Agent: *agent, Context: *task}
	return &fakeSession{runtime: r}, nil
}

func (r *fakeRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
	sess, err := r.Start(ctx, agent, task)
	if err != nil {
		return nil, err
	}
	events := make(chan launcherevents.Event)
	close(events)
	return sess.Run(ctx, events)
}

type fakeSession struct {
	runtime *fakeRuntime
}

func (s *fakeSession) Run(ctx context.Context, _ chan<- launcherevents.Event) (*launcher.Result, error) {
	timer := time.NewTimer(s.runtime.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return s.runtime.result, s.runtime.err
	}
}

func (s *fakeSession) SetApprover(launcher.Approver) error { return launcher.ErrNotSupported }

func (s *fakeSession) Close() error { return nil }

func startHTTPServer(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	return startHTTPServerOnListener(t, ln, handler)
}

func startHTTPServerOnAddr(t *testing.T, addr string, handler http.Handler) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("Listen(%s): %v", addr, err)
	}
	return startHTTPServerOnListener(t, ln, handler)
}

func startHTTPServerOnListener(t *testing.T, ln net.Listener, handler http.Handler) (string, func()) {
	t.Helper()

	srv := &http.Server{Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	return ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve: %v", err)
		}
	}
}
