package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

type failingWorkspaceManager struct {
	err error
}

func (m *failingWorkspaceManager) Create(_ int, _ string) (string, error) {
	return "", m.err
}

func (m *failingWorkspaceManager) Remove(string) error {
	return nil
}

func (m *failingWorkspaceManager) Prune() error {
	return nil
}

// initGitRepo creates a bare-minimum git repo in a temp directory for worker tests
// that need workspace isolation.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	marker := filepath.Join(dir, "README.md")
	if err := os.WriteFile(marker, []byte("test repo"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = dir
	_ = cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s: %v", out, err)
	}
	return dir
}

func TestParseWorkerFlags(t *testing.T) {
	cmd := &cobra.Command{Use: "worker"}
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().String("token", "", "")
	cmd.Flags().String("role", "", "")
	cmd.Flags().String("runtime", config.RuntimeClaudeCode, "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("repos", "", "")
	cmd.Flags().String("id", "", "")
	cmd.Flags().String("mgmt-addr", defaultWorkerMgmtAddr, "")
	cmd.Flags().Int("concurrency", 1, "")
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
	if err := cmd.Flags().Set("repos", "owner/a=/tmp/a,owner/b=/tmp/b"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("id", "worker-a"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("mgmt-addr", "127.0.0.1:9998"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("concurrency", "4"); err != nil {
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
	if opts.reposCSV != "owner/a=/tmp/a,owner/b=/tmp/b" || opts.workerID != "worker-a" || opts.mgmtAddr != "127.0.0.1:9998" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
	if opts.concurrency != 4 {
		t.Fatalf("expected concurrency=4, got %d", opts.concurrency)
	}
}

func TestParseWorkerFlagsFallsBackToEnvToken(t *testing.T) {
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "env-secret")

	cmd := &cobra.Command{Use: "worker"}
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().String("token", "", "")
	cmd.Flags().String("role", "", "")
	cmd.Flags().String("runtime", config.RuntimeClaudeCode, "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("repos", "", "")
	cmd.Flags().String("id", "", "")
	cmd.Flags().String("mgmt-addr", defaultWorkerMgmtAddr, "")
	cmd.Flags().Int("concurrency", 1, "")
	if err := cmd.Flags().Set("coordinator", "http://localhost:9999"); err != nil {
		t.Fatal(err)
	}

	opts, err := parseWorkerFlags(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := opts.token, "env-secret"; got != want {
		t.Fatalf("opts.token = %q, want %q", got, want)
	}
}

func TestParseWorkerFlagsRequiresTokenWhenFlagAndEnvMissing(t *testing.T) {
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "")

	cmd := &cobra.Command{Use: "worker"}
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().String("token", "", "")
	cmd.Flags().String("role", "", "")
	cmd.Flags().String("runtime", config.RuntimeClaudeCode, "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("repos", "", "")
	cmd.Flags().String("id", "", "")
	cmd.Flags().String("mgmt-addr", defaultWorkerMgmtAddr, "")
	cmd.Flags().Int("concurrency", 1, "")
	if err := cmd.Flags().Set("coordinator", "http://localhost:9999"); err != nil {
		t.Fatal(err)
	}

	_, err := parseWorkerFlags(cmd)
	if err == nil {
		t.Fatal("expected missing token to fail")
	}
	if !strings.Contains(err.Error(), "--token or WORKBUDDY_AUTH_TOKEN is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWorkerUnregisterCmd(t *testing.T) {
	var method string
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"unregistered"}`))
	}))
	defer srv.Close()

	cmd := &cobra.Command{Use: "unregister"}
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().String("token", "", "")
	cmd.Flags().String("id", "", "")
	if err := cmd.Flags().Set("coordinator", srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("token", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("id", "worker-1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd.SetContext(ctx)

	if err := runWorkerUnregister(cmd, nil); err != nil {
		t.Fatalf("runWorkerUnregister: %v", err)
	}
	if method != http.MethodDelete {
		t.Fatalf("method = %s, want DELETE", method)
	}
	if path != "/api/v1/workers/worker-1" {
		t.Fatalf("path = %s, want /api/v1/workers/worker-1", path)
	}
}

func TestWorkerUnregisterCmdNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"worker not found"}`))
	}))
	defer srv.Close()

	cmd := &cobra.Command{Use: "unregister"}
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().String("token", "", "")
	cmd.Flags().String("id", "", "")
	if err := cmd.Flags().Set("coordinator", srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("token", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("id", "missing-worker"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd.SetContext(ctx)

	err := runWorkerUnregister(cmd, nil)
	if err == nil {
		t.Fatal("expected error for 404")
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
		workDir:           initGitRepo(t),
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
			workDir:           initGitRepo(t),
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
			workDir:           initGitRepo(t),
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

func TestWorkerUsesMappedRepoPathAndCleansAddrFile(t *testing.T) {
	setupFakeGHCLI(t)

	repo := "owner/test-repo"
	repoPath := initGitRepo(t)
	controlDir := initGitRepo(t)
	configDir := setupTestConfigDir(t, repo)
	addrFile := workerAddrFile(controlDir)

	var registerReq workerclient.RegisterRequest
	taskCompleted := make(chan struct{})
	var pollCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workers/register":
			if err := json.NewDecoder(r.Body).Decode(&registerReq); err != nil {
				t.Fatalf("decode register: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/poll":
			if pollCount.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"task_id":"task-1","repo":"owner/test-repo","issue_num":12,"agent_name":"dev-agent"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/heartbeat"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result"):
			close(taskCompleted)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"completed"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	gh := &mockGHReader{
		issues: []poller.Issue{
			{Number: 12, Title: "Mapped Repo", State: "open", Body: "body", Labels: []string{"workbuddy", "status:developing"}},
		},
		labelSnapshots: [][]string{{"workbuddy", "status:done"}},
	}

	mockRT := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(_ context.Context, _ *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
		// With workspace isolation, WorkDir/RepoRoot are the worktree path.
		if !strings.HasPrefix(task.RepoRoot, repoPath) || !strings.HasPrefix(task.WorkDir, repoPath) {
			t.Fatalf("task context paths = %q / %q, expected prefix %q", task.RepoRoot, task.WorkDir, repoPath)
		}
		return &launcher.Result{ExitCode: 0, LastMessage: "done", Meta: map[string]string{}}, nil
	}}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT, config.RuntimeClaudeCode)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- runWorkerWithOpts(&workerOpts{
			coordinatorURL:    server.URL,
			token:             "",
			roleCSV:           "dev",
			runtime:           config.RuntimeClaudeCode,
			reposCSV:          repo + "=" + repoPath,
			configDir:         configDir,
			workDir:           controlDir,
			dbPath:            filepath.Join(controlDir, ".workbuddy", "worker.db"),
			sessionsDir:       filepath.Join(controlDir, ".workbuddy", "sessions"),
			pollTimeout:       20 * time.Millisecond,
			heartbeatInterval: 20 * time.Millisecond,
			shutdownTimeout:   100 * time.Millisecond,
		}, lnch, gh, ctx)
	}()

	select {
	case <-taskCompleted:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not complete mapped repo task")
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

	wantWorkerID := hostnameOrUnknown()
	if registerReq.WorkerID != wantWorkerID {
		t.Fatalf("worker_id = %q, want %q", registerReq.WorkerID, wantWorkerID)
	}
	if len(registerReq.Repos) != 1 || registerReq.Repos[0] != repo {
		t.Fatalf("unexpected registered repos: %+v", registerReq.Repos)
	}
	if _, err := os.Stat(addrFile); !os.IsNotExist(err) {
		t.Fatalf("expected worker addr cleanup, stat err = %v", err)
	}
}

func TestWorkerReleasesUnmappedTask(t *testing.T) {
	setupFakeGHCLI(t)

	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)

	released := make(chan string, 1)
	var releasedReason string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workers/register":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/poll":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"task_id":"task-1","repo":"owner/other-repo","issue_num":13,"agent_name":"dev-agent"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			var req workerclient.ReleaseRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode release: %v", err)
			}
			releasedReason = req.Reason
			released <- req.WorkerID
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- runWorkerWithOpts(&workerOpts{
			coordinatorURL:    server.URL,
			token:             "",
			roleCSV:           "dev",
			runtime:           config.RuntimeClaudeCode,
			repo:              repo,
			configDir:         configDir,
			workDir:           initGitRepo(t),
			dbPath:            filepath.Join(t.TempDir(), "worker.db"),
			sessionsDir:       filepath.Join(t.TempDir(), "sessions"),
			pollTimeout:       20 * time.Millisecond,
			heartbeatInterval: 20 * time.Millisecond,
			shutdownTimeout:   100 * time.Millisecond,
		}, launcher.NewLauncher(), &mockGHReader{}, ctx)
	}()

	select {
	case workerID := <-released:
		if workerID == "" {
			t.Fatal("expected release worker id")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not release unmapped task")
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
	if releasedReason != "repo not bound on worker" {
		t.Fatalf("release reason = %q", releasedReason)
	}
}

func TestWorkerDynamicRepoAddUpdatesCoordinatorAndDispatchesTask(t *testing.T) {
	setupFakeGHCLI(t)
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "secret-token")

	repoA := "owner/repo-a"
	repoB := "owner/repo-b"
	controlDir := initGitRepo(t)
	repoAPath := initGitRepo(t)
	repoBPath := initGitRepo(t)
	configDir := setupNamedConfigDir(t, repoA, "dev-agent", "workflow")
	configDirB := setupNamedConfigDir(t, repoB, "dev-agent", "workflow")
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &repoAwareGHReader{
		issuesByRepo: map[string][]poller.Issue{
			repoB: {{Number: 21, Title: "Repo B Task", State: "open", Body: "body-b", Labels: []string{"workbuddy", "status:developing"}}},
		},
	}

	ctxCoordinator, cancelCoordinator := context.WithCancel(context.Background())
	defer cancelCoordinator()
	coordErrCh := make(chan error, 1)
	go func() {
		coordErrCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 50 * time.Millisecond,
			dbPath:       dbPath,
			auth:         true,
		}, gh, ctxCoordinator)
	}()
	waitForHealth(t, port)

	client := &http.Client{Timeout: 5 * time.Second}
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port), "secret-token", mustRegistrationRequest(t, configDir))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register repo A status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp = postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port), "secret-token", mustRegistrationRequest(t, configDirB))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register repo B status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	taskCompleted := make(chan struct{})
	mockRT := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(_ context.Context, _ *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
		// With workspace isolation, WorkDir/RepoRoot are the worktree path under repoBPath.
		if task.Repo != repoB || !strings.HasPrefix(task.WorkDir, repoBPath) || !strings.HasPrefix(task.RepoRoot, repoBPath) {
			t.Fatalf("unexpected task context: repo=%s workdir=%s reporoot=%s", task.Repo, task.WorkDir, task.RepoRoot)
		}
		select {
		case <-taskCompleted:
		default:
			close(taskCompleted)
		}
		return &launcher.Result{ExitCode: 0, LastMessage: "done", Meta: map[string]string{}}, nil
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
			reposCSV:          repoA + "=" + repoAPath,
			configDir:         configDir,
			workDir:           controlDir,
			dbPath:            filepath.Join(controlDir, ".workbuddy", "worker.db"),
			sessionsDir:       filepath.Join(controlDir, ".workbuddy", "sessions"),
			pollTimeout:       100 * time.Millisecond,
			heartbeatInterval: 50 * time.Millisecond,
			shutdownTimeout:   time.Second,
		}, lnch, gh, ctxWorker)
	}()

	var mgmtClient *workerMgmtClient
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		mgmtClient, err = workerMgmtClientFromControlDir(controlDir)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if mgmtClient == nil {
		t.Fatal("worker mgmt client did not become available")
	}
	if err := mgmtClient.Add(context.Background(), workerRepoBinding{Repo: repoB, Path: repoBPath}); err != nil {
		t.Fatalf("mgmt add repo B: %v", err)
	}

	var workerHasRepoB bool
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		workers, err := st.QueryWorkers("")
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, worker := range workers {
			if worker.ID == hostnameOrUnknown() && strings.Contains(worker.ReposJSON, repoB) {
				workerHasRepoB = true
				break
			}
		}
		if workerHasRepoB {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !workerHasRepoB {
		t.Fatal("coordinator worker record was not updated with repo B")
	}

	select {
	case <-taskCompleted:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not execute dynamically added repo task")
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
	if _, err := os.Stat(workerAddrFile(controlDir)); !os.IsNotExist(err) {
		t.Fatalf("expected worker addr cleanup, stat err = %v", err)
	}

	cancelCoordinator()
	select {
	case err := <-coordErrCh:
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
	case <-time.After(12 * time.Second):
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
		nil, // wsMgr — no worktree isolation in test
	); err != nil {
		t.Fatalf("executeRemoteTask: %v", err)
	}

	afterRun := heartbeatCount.Load()
	time.Sleep(100 * time.Millisecond)
	if heartbeatCount.Load() != afterRun {
		t.Fatalf("heartbeat goroutine kept running after process exit: before=%d after=%d", afterRun, heartbeatCount.Load())
	}
}

func TestExecuteRemoteTaskRequeuesWorktreeSetupFailure(t *testing.T) {
	var (
		releaseReq   workerclient.ReleaseRequest
		releaseCount int
		resultCount  int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			releaseCount++
			if err := json.NewDecoder(r.Body).Decode(&releaseReq); err != nil {
				t.Fatalf("decode release: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result"):
			resultCount++
			w.WriteHeader(http.StatusOK)
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
		TaskID:    "task-worktree-fail",
		Repo:      "owner/test-repo",
		IssueNum:  14,
		AgentName: "dev-agent",
		Workflow:  "workflow",
	}
	reader := &mockGHReader{
		issues: []poller.Issue{
			{Number: 14, Body: "body", Labels: []string{"workbuddy", "status:developing"}},
		},
		labelSnapshots: [][]string{
			{"workbuddy", "status:developing"},
		},
	}
	comments := &mockCommentWriter{}
	rep := reporter.NewReporter(comments)
	auditor := audit.NewAuditor(nil, filepath.Join(t.TempDir(), "sessions"))
	client := workerclient.New(server.URL, "", server.Client())
	wsErr := errors.New("missing but already registered worktree")

	err := executeRemoteTask(
		context.Background(),
		task,
		client,
		cfg,
		launcher.NewLauncher(),
		auditor,
		rep,
		reader,
		t.TempDir(),
		filepath.Join(t.TempDir(), "sessions"),
		"worker-1",
		"",
		20*time.Millisecond,
		200*time.Millisecond,
		&failingWorkspaceManager{err: wsErr},
	)
	if err == nil || !strings.Contains(err.Error(), "worktree setup failed") {
		t.Fatalf("executeRemoteTask error = %v, want worktree setup failure", err)
	}
	if releaseCount != 1 {
		t.Fatalf("release count = %d, want 1", releaseCount)
	}
	if resultCount != 0 {
		t.Fatalf("result count = %d, want 0", resultCount)
	}
	if releaseReq.WorkerID != "worker-1" {
		t.Fatalf("release worker_id = %q, want worker-1", releaseReq.WorkerID)
	}
	if !strings.Contains(releaseReq.Reason, wsErr.Error()) {
		t.Fatalf("release reason = %q, want to contain %q", releaseReq.Reason, wsErr.Error())
	}

	allComments := comments.Comments()
	if len(allComments) != 1 {
		t.Fatalf("comment count = %d, want 1", len(allComments))
	}
	if !strings.Contains(allComments[0], wsErr.Error()) {
		t.Fatalf("comment missing worktree error: %s", allComments[0])
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
			workDir:           initGitRepo(t),
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

// Regression for #143: the heartbeat loop must recognize
// "ownership lost" error strings returned by the coordinator so it can
// cancel the local task context. Without this, a zombie goroutine keeps
// running (and potentially writing to a worktree already being claimed
// by a newer goroutine).
func TestIsTaskOwnershipLost(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"worker mismatch", errors.New(`workerclient: unexpected status 400: {"error":"worker_id does not match claimed task"}`), true},
		{"already completed", errors.New("store: complete task: task already completed"), true},
		{"not claimable", errors.New(`workerclient: unexpected status 409: {"error":"task is not claimable by this worker"}`), true},
		{"no longer owned", errors.New("store: complete task: task is no longer owned by worker or lease expired"), true},
		{"unrelated", errors.New("connection refused"), false},
		{"http 500", errors.New(`workerclient: unexpected status 500: {"error":"internal"}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isTaskOwnershipLost(tc.err)
			if got != tc.want {
				t.Fatalf("isTaskOwnershipLost(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
