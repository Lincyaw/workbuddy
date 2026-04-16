package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
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
		tasks, err := st.QueryTasks("")
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) > 0 && tasks[0].Status == store.TaskStatusCompleted {
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
