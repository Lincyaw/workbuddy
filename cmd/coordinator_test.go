package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestCoordinatorSIGTERMReleasesIssueClaimsHelper(t *testing.T) {
	if os.Getenv("WORKBUDDY_SIGTERM_HELPER") != "1" {
		t.Skip("helper process")
	}

	port, err := strconv.Atoi(os.Getenv("WORKBUDDY_SIGTERM_PORT"))
	if err != nil {
		t.Fatalf("parse helper port: %v", err)
	}
	repo := os.Getenv("WORKBUDDY_SIGTERM_REPO")
	dbPath := os.Getenv("WORKBUDDY_SIGTERM_DB")
	if repo == "" || dbPath == "" {
		t.Fatal("helper repo/db env is required")
	}

	gh := &repoAwareGHReader{
		issuesByRepo: map[string][]poller.Issue{
			repo: {{
				Number: 88,
				Title:  "Held claim",
				State:  "open",
				Body:   "body",
				Labels: []string{"workbuddy", "status:developing"},
			}},
		},
	}
	if err := runCoordinatorWithOpts(&coordinatorOpts{
		port:         port,
		pollInterval: 25 * time.Millisecond,
		dbPath:       dbPath,
	}, gh); err != nil {
		t.Fatalf("helper coordinator: %v", err)
	}
}

func TestCoordinatorWorkerUnregister(t *testing.T) {
	repo := "owner/repo"
	configDir := setupNamedConfigDir(t, repo, "dev-agent", "workflow")
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &repoAwareGHReader{
		issuesByRepo: map[string][]poller.Issue{
			repo: {{Number: 1, Title: "Test", State: "open", Body: "body", Labels: []string{"workbuddy", "status:developing"}}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 50 * time.Millisecond,
			dbPath:       dbPath,
		}, gh, ctx)
	}()
	waitForHealth(t, port)

	client := &http.Client{Timeout: 5 * time.Second}

	// Register repo.
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port), "", mustRegistrationRequest(t, configDir))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register repo status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Register worker.
	workerResp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/workers/register", port), "", workerRegisterRequest{
		WorkerID: "worker-1",
		Repo:     repo,
		Repos:    []string{repo},
		Roles:    []string{"dev"},
		Hostname: "host1",
	})
	if workerResp.StatusCode != http.StatusCreated {
		t.Fatalf("register worker status = %d", workerResp.StatusCode)
	}
	_ = workerResp.Body.Close()

	// Unregister worker should succeed.
	req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("http://localhost:%d/api/v1/workers/worker-1", port), nil)
	unregisterResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unregister request: %v", err)
	}
	if unregisterResp.StatusCode != http.StatusOK {
		t.Fatalf("unregister status = %d", unregisterResp.StatusCode)
	}
	_ = unregisterResp.Body.Close()

	// Poll with unregistered worker should fail with unknown worker.
	pollResp, err := client.Get(fmt.Sprintf("http://localhost:%d/api/v1/tasks/poll?worker_id=worker-1&timeout=100ms", port))
	if err != nil {
		t.Fatalf("poll after unregister: %v", err)
	}
	if pollResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("poll after unregister status = %d, want %d", pollResp.StatusCode, http.StatusBadRequest)
	}
	_ = pollResp.Body.Close()

	// Heartbeat with unregistered worker should also fail with unknown worker.
	if err := insertClaimedTaskForWorker(dbPath, repo, "task-heartbeat", 1, "worker-1"); err != nil {
		t.Fatalf("insert claimed task: %v", err)
	}
	heartbeatResp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/tasks/task-heartbeat/heartbeat", port), "", taskHeartbeatRequest{
		WorkerID: "worker-1",
	})
	if heartbeatResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("heartbeat after unregister status = %d, want %d", heartbeatResp.StatusCode, http.StatusBadRequest)
	}
	_ = heartbeatResp.Body.Close()

	// Unregister unknown worker should return 404.
	req, _ = http.NewRequest(http.MethodDelete, fmt.Sprintf("http://localhost:%d/api/v1/workers/unknown-worker", port), nil)
	unregisterResp, err = client.Do(req)
	if err != nil {
		t.Fatalf("unregister unknown request: %v", err)
	}
	if unregisterResp.StatusCode != http.StatusNotFound {
		t.Fatalf("unregister unknown status = %d, want %d", unregisterResp.StatusCode, http.StatusNotFound)
	}
	_ = unregisterResp.Body.Close()
}

func TestCoordinatorWorkerUnregisterWithRunningTask(t *testing.T) {
	repo := "owner/repo"
	configDir := setupNamedConfigDir(t, repo, "dev-agent", "workflow")
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &repoAwareGHReader{
		issuesByRepo: map[string][]poller.Issue{
			repo: {{Number: 1, Title: "Test", State: "open", Body: "body", Labels: []string{"workbuddy", "status:developing"}}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 50 * time.Millisecond,
			dbPath:       dbPath,
		}, gh, ctx)
	}()
	waitForHealth(t, port)

	client := &http.Client{Timeout: 5 * time.Second}

	// Register repo.
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port), "", mustRegistrationRequest(t, configDir))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register repo status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Register worker.
	workerResp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/workers/register", port), "", workerRegisterRequest{
		WorkerID: "worker-task",
		Repo:     repo,
		Repos:    []string{repo},
		Roles:    []string{"dev"},
		Hostname: "host1",
	})
	if workerResp.StatusCode != http.StatusCreated {
		t.Fatalf("register worker status = %d", workerResp.StatusCode)
	}
	_ = workerResp.Body.Close()

	// Manually insert and claim a task for the worker.
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-1",
		Repo:      repo,
		IssueNum:  1,
		AgentName: "dev-agent",
		Role:      "dev",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	claimed, err := st.ClaimTask("task-1", "worker-task")
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim to succeed")
	}
	_ = st.Close()

	// Unregister should be rejected with conflict.
	req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("http://localhost:%d/api/v1/workers/worker-task", port), nil)
	unregisterResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unregister request: %v", err)
	}
	if unregisterResp.StatusCode != http.StatusConflict {
		t.Fatalf("unregister with running task status = %d, want %d", unregisterResp.StatusCode, http.StatusConflict)
	}
	_ = unregisterResp.Body.Close()
}

func insertClaimedTaskForWorker(dbPath, repo, taskID string, issueNum int, workerID string) error {
	st, err := store.NewStore(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.InsertTask(store.TaskRecord{
		ID:        taskID,
		Repo:      repo,
		IssueNum:  issueNum,
		AgentName: "dev-agent",
		Role:      "dev",
		Status:    store.TaskStatusPending,
	}); err != nil {
		return err
	}
	claimed, err := st.ClaimTask(taskID, workerID)
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("claim task %s: expected claim to succeed", taskID)
	}
	return nil
}

func TestCoordinatorSIGTERMReleasesIssueClaims(t *testing.T) {
	repo := "owner/repo"
	configDir := setupNamedConfigDir(t, repo, "dev-agent", "workflow")
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "sigterm-coordinator.db")

	cmd := exec.Command(os.Args[0], "-test.run", "^TestCoordinatorSIGTERMReleasesIssueClaimsHelper$")
	cmd.Env = append(os.Environ(),
		"WORKBUDDY_SIGTERM_HELPER=1",
		"WORKBUDDY_SIGTERM_PORT="+strconv.Itoa(port),
		"WORKBUDDY_SIGTERM_REPO="+repo,
		"WORKBUDDY_SIGTERM_DB="+dbPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper coordinator: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	waitForHealth(t, port)

	client := &http.Client{Timeout: 5 * time.Second}
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port), "", mustRegistrationRequest(t, configDir))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register repo status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	waitForIssueClaim(t, dbPath, repo, 88)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal coordinator: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait helper coordinator: %v", err)
	}

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	claim, err := st.QueryIssueClaim(repo, 88)
	if err != nil {
		t.Fatalf("QueryIssueClaim: %v", err)
	}
	if claim != nil {
		t.Fatalf("expected SIGTERM shutdown to release claim, got %+v", claim)
	}
}

func waitForIssueClaim(t *testing.T, dbPath, repo string, issueNum int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := store.NewStore(dbPath)
		if err == nil {
			claim, qErr := st.QueryIssueClaim(repo, issueNum)
			_ = st.Close()
			if qErr == nil && claim != nil {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for issue claim %s#%d", repo, issueNum)
}
