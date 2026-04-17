package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/workerclient"
	"github.com/spf13/cobra"
)

func TestParseWorkerFlags(t *testing.T) {
	cmd := &cobra.Command{Use: "worker"}
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().String("token", "", "")
	cmd.Flags().String("role", "", "")
	cmd.Flags().String("runtime", config.RuntimeClaudeCode, "")
	cmd.Flags().String("repo", "", "")
	if err := cmd.Flags().Set("coordinator", "http://localhost:9999"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("token", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("role", "dev,review"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("runtime", "codex"); err != nil {
		t.Fatal(err)
	}

	opts, err := parseWorkerFlags(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if opts.coordinatorURL != "http://localhost:9999" || opts.token != "secret" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
	if opts.roleCSV != "dev,review" || opts.runtime != "codex" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
}

func TestWorkerRejectsInvalidToken(t *testing.T) {
	setupFakeGHCLI(t)
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "secret-token")

	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	port := 18944
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: time.Second,
			configDir:    configDir,
			dbPath:       dbPath,
			auth:         true,
		}, &mockGHReader{}, ctx)
	}()
	waitForHealth(t, port)

	err := runWorkerWithOpts(&workerOpts{
		coordinatorURL:    "http://localhost:18944",
		token:             "bad-token",
		runtime:           config.RuntimeClaudeCode,
		repo:              repo,
		configDir:         configDir,
		workDir:           t.TempDir(),
		dbPath:            filepath.Join(t.TempDir(), "worker.db"),
		sessionsDir:       filepath.Join(t.TempDir(), "sessions"),
		pollTimeout:       100 * time.Millisecond,
		heartbeatInterval: 50 * time.Millisecond,
		shutdownTimeout:   time.Second,
	}, launcher.NewLauncher(), &mockGHReader{})
	if err == nil {
		t.Fatal("expected invalid token error")
	}
	if !strings.Contains(err.Error(), "rejected the provided token") {
		t.Fatalf("unexpected error: %v", err)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit")
	}
}

func TestWorkerPairsWithCoordinatorAndCompletesTask(t *testing.T) {
	setupFakeGHCLI(t)
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "secret-token")

	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &mockGHReader{
		issues: []poller.Issue{
			{Number: 7, Title: "Synthetic Task", State: "open", Body: "body", Labels: []string{"workbuddy", "status:developing"}},
		},
		labelSnapshots: [][]string{
			{"workbuddy", "status:done"},
		},
	}

	ctxCoordinator, cancelCoordinator := context.WithCancel(context.Background())
	defer cancelCoordinator()
	coordErrCh := make(chan error, 1)
	go func() {
		coordErrCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 50 * time.Millisecond,
			configDir:    configDir,
			dbPath:       dbPath,
			auth:         true,
		}, gh, ctxCoordinator)
	}()
	waitForHealth(t, port)

	mockRT := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(_ context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
		return &launcher.Result{
			ExitCode:    0,
			LastMessage: "done",
			Meta:        map[string]string{},
		}, nil
	}}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT, config.RuntimeClaudeCode)

	ctxWorker, cancelWorker := context.WithCancel(context.Background())
	workerErrCh := make(chan error, 1)
	go func() {
		workerErrCh <- runWorkerWithOpts(&workerOpts{
			coordinatorURL:    fmt.Sprintf("http://localhost:%d", port),
			token:             "secret-token",
			roleCSV:           "dev",
			runtime:           config.RuntimeClaudeCode,
			repo:              repo,
			configDir:         configDir,
			workDir:           t.TempDir(),
			dbPath:            filepath.Join(t.TempDir(), "worker.db"),
			sessionsDir:       filepath.Join(t.TempDir(), "sessions"),
			pollTimeout:       100 * time.Millisecond,
			heartbeatInterval: 50 * time.Millisecond,
			shutdownTimeout:   time.Second,
		}, lnch, gh, ctxWorker)
	}()

	var completed bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		tasks, err := st.QueryTasks(store.TaskStatusCompleted)
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) > 0 {
			completed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !completed {
		t.Fatal("worker did not complete synthetic task")
	}

	cancelWorker()
	select {
	case err := <-workerErrCh:
		if err != nil {
			t.Fatalf("worker: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not exit")
	}

	cancelCoordinator()
	select {
	case err := <-coordErrCh:
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit")
	}

	if mockRT.CallCount() == 0 {
		t.Fatal("expected worker runtime to execute task")
	}
}

func TestWorkerShutdownRequeuesInFlightTask(t *testing.T) {
	setupFakeGHCLI(t)
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "secret-token")

	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	port := 18946
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &mockGHReader{
		issues: []poller.Issue{
			{Number: 8, Title: "Slow Task", State: "open", Body: "body", Labels: []string{"workbuddy", "status:developing"}},
		},
		labelSnapshots: [][]string{
			{"workbuddy", "status:developing"},
		},
	}

	ctxCoordinator, cancelCoordinator := context.WithCancel(context.Background())
	defer cancelCoordinator()
	coordErrCh := make(chan error, 1)
	go func() {
		coordErrCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 50 * time.Millisecond,
			configDir:    configDir,
			dbPath:       dbPath,
			auth:         true,
		}, gh, ctxCoordinator)
	}()
	waitForHealth(t, port)

	started := make(chan struct{})
	mockRT := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(ctx context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT, config.RuntimeClaudeCode)

	ctxWorker, cancelWorker := context.WithCancel(context.Background())
	workerErrCh := make(chan error, 1)
	go func() {
		workerErrCh <- runWorkerWithOpts(&workerOpts{
			coordinatorURL:    "http://localhost:18946",
			token:             "secret-token",
			roleCSV:           "dev",
			runtime:           config.RuntimeClaudeCode,
			repo:              repo,
			configDir:         configDir,
			workDir:           t.TempDir(),
			dbPath:            filepath.Join(t.TempDir(), "worker.db"),
			sessionsDir:       filepath.Join(t.TempDir(), "sessions"),
			pollTimeout:       100 * time.Millisecond,
			heartbeatInterval: 50 * time.Millisecond,
			shutdownTimeout:   time.Second,
		}, lnch, gh, ctxWorker)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start task")
	}

	cancelWorker()
	select {
	case err := <-workerErrCh:
		if err != nil {
			t.Fatalf("worker: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not exit")
	}

	var requeued bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		tasks, err := st.QueryTasks("")
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) > 0 && tasks[0].Status == store.TaskStatusPending && tasks[0].WorkerID == "" {
			requeued = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !requeued {
		t.Fatal("expected in-flight task to be requeued on shutdown")
	}

	cancelCoordinator()
	select {
	case err := <-coordErrCh:
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("coordinator did not exit")
	}
}

func TestExecuteRemoteTaskStopsHeartbeatAfterKilledProcess(t *testing.T) {
	localStore, err := store.NewStore(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = localStore.Close() }()

	var heartbeatCount atomic.Int64
	firstHeartbeat := make(chan struct{})
	var firstHeartbeatOnce sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			heartbeatCount.Add(1)
			firstHeartbeatOnce.Do(func() {
				close(firstHeartbeat)
			})
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/result"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"failed"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := &config.FullConfig{
		Agents: map[string]*config.AgentConfig{
			"dev-agent": {
				Name:    "dev-agent",
				Role:    "dev",
				Runtime: config.RuntimeClaudeCode,
				Command: "echo hello",
			},
		},
	}
	task := &workerclient.Task{
		TaskID:    "task-killed",
		Repo:      "owner/test-repo",
		IssueNum:  9,
		AgentName: "dev-agent",
	}
	reader := &mockGHReader{
		issues: []poller.Issue{
			{Number: 9, Body: "body", Labels: []string{"workbuddy", "status:developing"}},
		},
		labelSnapshots: [][]string{
			{"workbuddy", "status:developing"},
		},
	}

	mockRT := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(_ context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
		<-firstHeartbeat
		return nil, errors.New("signal: killed")
	}}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT, config.RuntimeClaudeCode)

	rep := reporter.NewReporter(&mockCommentWriter{})
	auditor := audit.NewAuditor(localStore, filepath.Join(t.TempDir(), "sessions"))
	client := workerclient.New(server.URL, "", server.Client())

	if err := executeRemoteTask(
		context.Background(),
		task,
		client,
		cfg,
		lnch,
		auditor,
		rep,
		reader,
		t.TempDir(),
		filepath.Join(t.TempDir(), "sessions"),
		"worker-1",
		"",
		20*time.Millisecond,
		200*time.Millisecond,
	); err != nil {
		t.Fatalf("executeRemoteTask: %v", err)
	}

	afterRun := heartbeatCount.Load()
	time.Sleep(100 * time.Millisecond)
	if heartbeatCount.Load() != afterRun {
		t.Fatalf("heartbeat goroutine kept running after process exit: before=%d after=%d", afterRun, heartbeatCount.Load())
	}
}

func TestWorkerRecoversAfterKilledTaskWhenResultSubmitFails(t *testing.T) {
	setupFakeGHCLI(t)

	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)

	var pollCount atomic.Int64
	var task1SubmitCount atomic.Int64
	var task2SubmitCount atomic.Int64
	secondTaskCompleted := make(chan struct{})
	var secondTaskOnce sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workers/register":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/poll":
			switch pollCount.Add(1) {
			case 1:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"task_id":"task-1","repo":"owner/test-repo","issue_num":10,"agent_name":"dev-agent"}`))
			case 2:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"task_id":"task-2","repo":"owner/test-repo","issue_num":11,"agent_name":"dev-agent"}`))
			default:
				w.WriteHeader(http.StatusNoContent)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/heartbeat"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/task-1/result"):
			task1SubmitCount.Add(1)
			http.Error(w, `{"error":"task already completed"}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/task-2/result"):
			task2SubmitCount.Add(1)
			secondTaskOnce.Do(func() {
				close(secondTaskCompleted)
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"completed"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	gh := &mockGHReader{
		issues: []poller.Issue{
			{Number: 10, Title: "Killed Task", State: "open", Body: "body", Labels: []string{"workbuddy", "status:developing"}},
			{Number: 11, Title: "Next Task", State: "open", Body: "body", Labels: []string{"workbuddy", "status:developing"}},
		},
		labelSnapshots: [][]string{
			{"workbuddy", "status:developing"},
			{"workbuddy", "status:done"},
		},
	}

	var runCount atomic.Int64
	mockRT := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(_ context.Context, _ *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
		switch runCount.Add(1) {
		case 1:
			time.Sleep(60 * time.Millisecond)
			return &launcher.Result{ExitCode: 1, Meta: map[string]string{}}, errors.New("signal: killed")
		case 2:
			return &launcher.Result{ExitCode: 0, LastMessage: fmt.Sprintf("done for %d", task.Issue.Number), Meta: map[string]string{}}, nil
		default:
			return &launcher.Result{ExitCode: 0, Meta: map[string]string{}}, nil
		}
	}}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT, config.RuntimeClaudeCode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runWorkerWithOpts(&workerOpts{
			coordinatorURL:    server.URL,
			token:             "",
			roleCSV:           "dev",
			runtime:           config.RuntimeClaudeCode,
			repo:              repo,
			configDir:         configDir,
			workDir:           t.TempDir(),
			dbPath:            filepath.Join(t.TempDir(), "worker.db"),
			sessionsDir:       filepath.Join(t.TempDir(), "sessions"),
			pollTimeout:       20 * time.Millisecond,
			heartbeatInterval: 20 * time.Millisecond,
			shutdownTimeout:   100 * time.Millisecond,
		}, lnch, gh, ctx)
	}()

	select {
	case <-secondTaskCompleted:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not recover and submit the next task")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("worker: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not exit")
	}

	if mockRT.CallCount() < 2 {
		t.Fatalf("expected worker to execute two tasks, ran %d", mockRT.CallCount())
	}
	if task1SubmitCount.Load() == 0 {
		t.Fatal("expected failed submit attempt for killed task")
	}
	if task2SubmitCount.Load() != 1 {
		t.Fatalf("expected exactly one successful submit for next task, got %d", task2SubmitCount.Load())
	}
	if pollCount.Load() < 2 {
		t.Fatalf("expected worker to return to poll loop, got %d poll(s)", pollCount.Load())
	}
}
