package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestCoordinatorAuthEnforcesBearerTokens(t *testing.T) {
	setupFakeGHCLI(t)
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "secret-token")

	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	port := 18941
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 5 * time.Second,
			configDir:    configDir,
			dbPath:       dbPath,
			auth:         true,
		}, &mockGHReader{}, ctx)
	}()
	waitForHealth(t, port)

	registerBody := []byte(`{"worker_id":"worker-1","repo":"owner/test-repo","roles":["dev"]}`)

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://localhost:%d/api/v1/workers/register", port), bytes.NewReader(registerBody))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing bearer token status=%d want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	_ = resp.Body.Close()

	req, err = http.NewRequest(http.MethodPost, fmt.Sprintf("http://localhost:%d/api/v1/workers/register", port), bytes.NewReader(registerBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid bearer token status=%d want %d", resp.StatusCode, http.StatusCreated)
	}
	_ = resp.Body.Close()

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runCoordinatorWithOpts: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit")
	}
}

func TestCoordinatorFakeWorkerCompletesTaskCycle(t *testing.T) {
	setupFakeGHCLI(t)

	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	port := 18942
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &mockGHReader{
		issues: []poller.Issue{
			{Number: 7, Title: "Task", State: "open", Labels: []string{"workbuddy", "status:developing"}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 50 * time.Millisecond,
			configDir:    configDir,
			dbPath:       dbPath,
		}, gh, ctx)
	}()
	waitForHealth(t, port)

	registerWorker(t, port, "", workerRegisterRequest{
		WorkerID: "worker-1",
		Repo:     repo,
		Roles:    []string{"dev"},
		Hostname: "fake-host",
	})

	task := pollTask(t, port, "", "worker-1", 2*time.Second)
	if task.TaskID == "" {
		t.Fatal("expected a task from poll")
	}
	if task.Repo != repo || task.IssueNum != 7 || task.AgentName != "dev-agent" {
		t.Fatalf("unexpected task payload: %+v", task)
	}

	postJSON(t,
		fmt.Sprintf("http://localhost:%d/api/v1/tasks/%s/result", port, task.TaskID),
		"",
		taskResultRequest{
			WorkerID:      "worker-1",
			Status:        store.TaskStatusCompleted,
			CurrentLabels: []string{"workbuddy", "status:done"},
		},
		http.StatusOK,
		nil,
	)

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runCoordinatorWithOpts: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit")
	}

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	gotTask, err := st.GetTask(task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTask == nil {
		t.Fatal("expected stored task")
	}
	if gotTask.Status != store.TaskStatusCompleted {
		t.Fatalf("task status=%s want %s", gotTask.Status, store.TaskStatusCompleted)
	}
}

func TestCoordinatorShutdownDrainsLongPolls(t *testing.T) {
	setupFakeGHCLI(t)

	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	port := 18943

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 5 * time.Second,
			configDir:    configDir,
			dbPath:       filepath.Join(t.TempDir(), "coordinator.db"),
		}, &mockGHReader{}, ctx)
	}()
	waitForHealth(t, port)

	registerWorker(t, port, "", workerRegisterRequest{
		WorkerID: "worker-1",
		Repo:     repo,
		Roles:    []string{"dev"},
		Hostname: "fake-host",
	})

	resultCh := make(chan int, 1)
	go func() {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/api/v1/tasks/poll?worker_id=worker-1&timeout=10s", port))
		if err != nil {
			resultCh <- -1
			return
		}
		defer func() { _ = resp.Body.Close() }()
		resultCh <- resp.StatusCode
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case status := <-resultCh:
		if status != http.StatusNoContent {
			t.Fatalf("long poll status=%d want %d", status, http.StatusNoContent)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll did not drain on shutdown")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runCoordinatorWithOpts: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit")
	}
}

func waitForHealth(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", port))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("health endpoint on port %d did not become ready", port)
}

func registerWorker(t *testing.T, port int, bearer string, req workerRegisterRequest) {
	t.Helper()
	postJSON(t,
		fmt.Sprintf("http://localhost:%d/api/v1/workers/register", port),
		bearer,
		req,
		http.StatusCreated,
		nil,
	)
}

func pollTask(t *testing.T, port int, bearer, workerID string, timeout time.Duration) taskPollResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://localhost:%d/api/v1/tasks/poll?worker_id=%s&timeout=%s", port, workerID, timeout), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poll status=%d want %d", resp.StatusCode, http.StatusOK)
	}
	var out taskPollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func postJSON(t *testing.T, url, bearer string, body any, wantStatus int, out any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s status=%d want %d", url, resp.StatusCode, wantStatus)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}
