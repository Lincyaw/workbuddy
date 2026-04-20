package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestCoordinatorConfigReloadEndpointUpdatesPollIntervalAndLogsEvent(t *testing.T) {
	repo := "owner/reload-endpoint"
	configDir := setupNamedConfigDir(t, repo, "dev-agent", "workflow-reload")
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 50 * time.Millisecond,
			dbPath:       dbPath,
			configDir:    configDir,
		}, &repoAwareGHReader{issuesByRepo: map[string][]poller.Issue{repo: nil}}, ctx)
	}()
	waitForHealth(t, port)

	writeFile(t, filepath.Join(configDir, "config.yaml"), fmt.Sprintf("repo: %s\nenvironment: test\npoll_interval: 75ms\nport: 0\nnotifications:\n  enabled: false\n", repo))

	client := &http.Client{Timeout: 5 * time.Second}
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/config/reload", port), "", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload status = %d", resp.StatusCode)
	}
	var summary configReloadSummary
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		t.Fatalf("decode reload summary: %v", err)
	}
	_ = resp.Body.Close()

	if got, want := summary.PollInterval, "75ms"; got != want {
		t.Fatalf("poll interval = %q, want %q", got, want)
	}
	if !containsString(summary.Changed, "poll_interval") {
		t.Fatalf("changed fields = %v, want poll_interval", summary.Changed)
	}

	waitForEventType(t, dbPath, repo, eventlog.TypeConfigReloaded)

	cancel()
	waitForCoordinatorExit(t, errCh)
}

func TestCoordinatorConfigWatcherReloadsAgentConfigWithoutAffectingClaimedTask(t *testing.T) {
	repo := "owner/reload-watch"
	configDir := setupNamedConfigDir(t, repo, "dev-agent", "workflow-reload")
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &repoAwareGHReader{
		issuesByRepo: map[string][]poller.Issue{
			repo: {
				{Number: 11, Title: "first", State: "open", Body: "body", Labels: []string{"workbuddy", "status:developing"}},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 25 * time.Millisecond,
			dbPath:       dbPath,
			configDir:    configDir,
		}, gh, ctx)
	}()
	waitForHealth(t, port)

	task11 := waitForTaskByIssue(t, dbPath, 11)
	if got, want := task11.Runtime, config.RuntimeClaudeCode; got != want {
		t.Fatalf("initial task runtime = %q, want %q", got, want)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/workers/register", port), "", workerRegisterRequest{
		WorkerID: "worker-1",
		Repo:     repo,
		Repos:    []string{repo},
		Roles:    []string{"dev"},
		Hostname: "host-1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register worker status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	pollResp, err := client.Get(fmt.Sprintf("http://localhost:%d/api/v1/tasks/poll?worker_id=worker-1&timeout=1s", port))
	if err != nil {
		t.Fatalf("poll claimed task: %v", err)
	}
	if pollResp.StatusCode != http.StatusOK {
		t.Fatalf("poll claimed task status = %d", pollResp.StatusCode)
	}
	var claimed taskPollResponse
	if err := json.NewDecoder(pollResp.Body).Decode(&claimed); err != nil {
		t.Fatalf("decode claimed task: %v", err)
	}
	_ = pollResp.Body.Close()
	if claimed.IssueNum != 11 {
		t.Fatalf("claimed issue = %d, want 11", claimed.IssueNum)
	}

	writeFile(t, filepath.Join(configDir, "agents", "dev-agent.md"), `---
name: dev-agent
description: Dev agent
triggers:
  - label: "status:developing"
role: dev
runtime: codex
command: echo "hello"
timeout: 30s
---
# Agent
`)
	waitForEventType(t, dbPath, repo, eventlog.TypeConfigReloaded)

	gh.mu.Lock()
	gh.issuesByRepo[repo] = append(gh.issuesByRepo[repo], poller.Issue{
		Number: 12, Title: "second", State: "open", Body: "body-2", Labels: []string{"workbuddy", "status:developing"},
	})
	gh.mu.Unlock()

	task12 := waitForTaskByIssue(t, dbPath, 12)
	if got, want := task12.Runtime, config.RuntimeCodex; got != want {
		t.Fatalf("reloaded task runtime = %q, want %q", got, want)
	}

	claimedTask := waitForTaskID(t, dbPath, claimed.TaskID)
	if got, want := claimedTask.Runtime, config.RuntimeClaudeCode; got != want {
		t.Fatalf("claimed task runtime changed to %q, want %q", got, want)
	}
	if got := claimedTask.WorkerID; got != "worker-1" {
		t.Fatalf("claimed task worker_id = %q, want worker-1", got)
	}

	cancel()
	waitForCoordinatorExit(t, errCh)
}

func TestCoordinatorConfigReloadValidationErrorIsNonFatal(t *testing.T) {
	repo := "owner/reload-invalid"
	configDir := setupNamedConfigDir(t, repo, "dev-agent", "workflow-reload")
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 50 * time.Millisecond,
			dbPath:       dbPath,
			configDir:    configDir,
		}, &repoAwareGHReader{issuesByRepo: map[string][]poller.Issue{repo: nil}}, ctx)
	}()
	waitForHealth(t, port)

	writeFile(t, filepath.Join(configDir, "agents", "dev-agent.md"), `---
name: dev-agent
description: broken
triggers:
  - label: "status:developing"
role: dev
runtime: invalid-runtime
command: echo "broken"
---
# Agent
`)

	client := &http.Client{Timeout: 5 * time.Second}
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/config/reload", port), "", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid reload status = %d, want 400", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode invalid reload body: %v", err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(body["error"], "invalid runtime") {
		t.Fatalf("reload error = %q, want invalid runtime", body["error"])
	}

	healthResp := getCoordinator(t, client, fmt.Sprintf("http://localhost:%d/health", port), "")
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("health after invalid reload = %d", healthResp.StatusCode)
	}
	_ = healthResp.Body.Close()

	waitForEventType(t, dbPath, repo, eventlog.TypeConfigReloadFailed)

	cancel()
	waitForCoordinatorExit(t, errCh)
}

func waitForCoordinatorExit(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("coordinator exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit")
	}
}

func waitForTaskByIssue(t *testing.T, dbPath string, issueNum int) *store.TaskRecord {
	t.Helper()
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
		for i := range tasks {
			if tasks[i].IssueNum == issueNum {
				task := tasks[i]
				return &task
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task for issue #%d not found", issueNum)
	return nil
}

func waitForTaskID(t *testing.T, dbPath, taskID string) *store.TaskRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		task, err := st.GetTask(taskID)
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if task != nil {
			return task
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task %s not found", taskID)
	return nil
}

func waitForEventType(t *testing.T, dbPath, repo, eventType string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		events, err := st.QueryEvents(repo)
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == eventType {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("event type %q not found for repo %s", eventType, repo)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
