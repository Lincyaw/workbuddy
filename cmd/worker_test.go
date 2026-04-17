package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/poller"
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

func TestStartStaleInferenceMonitorKillsIdleAgent(t *testing.T) {
	artifactDir := filepath.Join(t.TempDir(), "session-1")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(artifactDir, "codex-exec.jsonl")
	if err := os.WriteFile(artifactPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(artifactPath, old, old); err != nil {
		t.Fatal(err)
	}

	session := &mockStaleSession{
		info: launcher.StaleInferenceInfo{
			PID:          4242,
			ArtifactPath: artifactPath,
		},
	}
	st := storeForWorkerTest(t)
	logger := eventlog.NewEventLogger(st)
	killed := make(chan int, 1)
	stop := startStaleInferenceMonitor(
		context.Background(),
		config.EffectiveStaleInferenceConfig{
			Enabled:       true,
			IdleThreshold: 500 * time.Millisecond,
			CheckInterval: 25 * time.Millisecond,
		},
		session,
		&workerclient.Task{TaskID: "task-1", Repo: "owner/repo", IssueNum: 7, AgentName: "dev-agent"},
		"worker-1",
		logger,
		staleInferenceMonitorDeps{
			hasRunningChildren: func(int) (bool, error) { return false, nil },
			killProcessGroup: func(pid int) error {
				killed <- pid
				return nil
			},
		},
	)
	time.Sleep(150 * time.Millisecond)
	kill := stop()
	if kill == nil {
		t.Fatal("expected stale inference kill")
	}
	if kill.PID != 4242 {
		t.Fatalf("pid = %d, want 4242", kill.PID)
	}
	select {
	case pid := <-killed:
		if pid != 4242 {
			t.Fatalf("killed pid = %d, want 4242", pid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected killProcessGroup call")
	}

	events, err := logger.Query(eventlog.EventFilter{Repo: "owner/repo", IssueNum: 7, Type: eventlog.TypeAgentStaleInference})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("stale inference events = %d, want 1", len(events))
	}
}

func TestStaleInferenceStatusUsesWorkflowLabels(t *testing.T) {
	cfg := &config.FullConfig{
		Workflows: map[string]*config.WorkflowConfig{
			"dev-workflow": {
				States: map[string]*config.State{
					"developing": {EnterLabel: "status:developing"},
					"reviewing":  {EnterLabel: "status:reviewing"},
				},
			},
		},
	}
	task := &workerclient.Task{Workflow: "dev-workflow", State: "developing"}
	if got := staleInferenceStatus(task, cfg, []string{"status:developing"}); got != store.TaskStatusFailed {
		t.Fatalf("status with unchanged label = %q, want failed", got)
	}
	if got := staleInferenceStatus(task, cfg, []string{"status:reviewing"}); got != store.TaskStatusCompleted {
		t.Fatalf("status with advanced label = %q, want completed", got)
	}
}

func TestWorkerKillsStaleInferenceAndCompletesTask(t *testing.T) {
	setupFakeGHCLI(t)
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "secret-token")

	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &mockGHReader{
		issues: []poller.Issue{
			{Number: 9, Title: "Hung Task", State: "open", Body: "body", Labels: []string{"workbuddy", "status:developing"}},
		},
		labelSnapshots: [][]string{
			{"workbuddy", "status:done"},
			{"workbuddy", "status:done"},
		},
	}
	writeFile(t, filepath.Join(configDir, "config.yaml"), "repo: "+repo+"\npoll_interval: 1s\nport: 0\nworker:\n  stale_inference:\n    enabled: true\n    idle_threshold: 250ms\n    check_interval: 25ms\n")

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

	lnch := launcher.NewLauncher()
	lnch.Register(&hangingProcessRuntime{name: config.RuntimeClaudeCode}, config.RuntimeClaudeCode)

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
		t.Fatal("worker did not recover hung task")
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
}

func storeForWorkerTest(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "worker-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
