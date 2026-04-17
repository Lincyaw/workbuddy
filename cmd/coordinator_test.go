package cmd

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

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

func TestParseCoordinatorFlagsRejectsNonLoopbackBypass(t *testing.T) {
	cmd := coordinatorCmd
	cmd.Flags().Set("listen", "0.0.0.0:8081")
	cmd.Flags().Set("loopback-only", "true")
	t.Cleanup(func() {
		cmd.Flags().Set("listen", "127.0.0.1:8081")
		cmd.Flags().Set("loopback-only", "false")
	})

	_, err := parseCoordinatorFlags(cmd)
	if err == nil {
		t.Fatal("expected error for non-loopback listen address")
	}
}

func TestParseCoordinatorFlagsAllowsLoopbackBypass(t *testing.T) {
	tests := []string{
		"127.0.0.1:8081",
		"[::1]:8081",
		"localhost:8081",
	}

	for _, listenAddr := range tests {
		t.Run(listenAddr, func(t *testing.T) {
			cmd := coordinatorCmd
			cmd.Flags().Set("listen", listenAddr)
			cmd.Flags().Set("loopback-only", "true")
			t.Cleanup(func() {
				cmd.Flags().Set("listen", "127.0.0.1:8081")
				cmd.Flags().Set("loopback-only", "false")
			})

			opts, err := parseCoordinatorFlags(cmd)
			if err != nil {
				t.Fatalf("parseCoordinatorFlags: %v", err)
			}
			if opts.listenAddr != listenAddr {
				t.Fatalf("listenAddr = %q, want %q", opts.listenAddr, listenAddr)
			}
			if !opts.loopbackOnly {
				t.Fatal("expected loopbackOnly to be true")
			}
		})
	}
}

func TestParseCoordinatorFlagsReadsNewOptions(t *testing.T) {
	cmd := coordinatorCmd
	cmd.Flags().Set("listen", "127.0.0.1:8081")
	cmd.Flags().Set("config-dir", " .github/workbuddy/ ")
	cmd.Flags().Set("port", "8123")
	cmd.Flags().Set("poll-interval", "42s")
	cmd.Flags().Set("auth", "true")
	cmd.Flags().Set("trusted-authors", "alice,bob")
	t.Cleanup(func() {
		cmd.Flags().Set("config-dir", ".github/workbuddy")
		cmd.Flags().Set("port", "0")
		cmd.Flags().Set("poll-interval", "0s")
		cmd.Flags().Set("auth", "false")
		cmd.Flags().Set("trusted-authors", "")
	})

	opts, err := parseCoordinatorFlags(cmd)
	if err != nil {
		t.Fatalf("parseCoordinatorFlags: %v", err)
	}
	if got, want := opts.configDir, ".github/workbuddy/"; got != want {
		t.Fatalf("configDir = %q, want %q", got, want)
	}
	if got, want := opts.port, 8123; got != want {
		t.Fatalf("port = %d, want %d", got, want)
	}
	if got, want := opts.pollInterval, 42*time.Second; got != want {
		t.Fatalf("pollInterval = %s, want %s", got, want)
	}
	if !opts.auth {
		t.Fatal("expected auth to be true")
	}
	if got, want := opts.trustedAuthors, "alice,bob"; got != want {
		t.Fatalf("trustedAuthors = %q, want %q", got, want)
	}
	if !opts.trustedAuthorsSet {
		t.Fatal("expected trustedAuthorsSet to be true")
	}
}
