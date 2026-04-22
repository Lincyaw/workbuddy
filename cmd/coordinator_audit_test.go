package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func seedCoordinatorAuditDB(t *testing.T, dbPath string) {
	t.Helper()

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.DB().Exec(
		`INSERT INTO issue_cache (repo, issue_num, labels, body, state) VALUES (?, ?, ?, ?, ?)`,
		"owner/repo-a", 11, `["workbuddy","status:developing"]`, "body", "open",
	); err != nil {
		t.Fatalf("insert issue_cache: %v", err)
	}

	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-11",
		Repo:      "owner/repo-a",
		IssueNum:  11,
		AgentName: "dev-agent",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	eventID, err := st.InsertEvent(store.Event{
		Type:     "dispatch",
		Repo:     "owner/repo-a",
		IssueNum: 11,
		Payload:  `{"task_id":"task-11"}`,
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE events SET ts = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), eventID); err != nil {
		t.Fatalf("update event ts: %v", err)
	}
}

func TestCoordinatorExposesAuditEndpoints(t *testing.T) {
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	seedCoordinatorAuditDB(t, dbPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: time.Second,
			dbPath:       dbPath,
		}, nil, ctx)
	}()
	waitForHealth(t, port)

	client := &http.Client{Timeout: 5 * time.Second}

	eventsResp := getCoordinator(t, client, fmt.Sprintf("http://localhost:%d/events?repo=owner/repo-a&type=dispatch&since=2000-01-01T00:00:00Z", port), "")
	defer func() { _ = eventsResp.Body.Close() }()
	if eventsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events status = %d", eventsResp.StatusCode)
	}
	var events audit.EventsResponse
	if err := json.NewDecoder(eventsResp.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(events.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(events.Events))
	}
	if got := events.Events[0].Type; got != "dispatch" {
		t.Fatalf("event type = %q, want dispatch", got)
	}

	tasksResp := getCoordinator(t, client, fmt.Sprintf("http://localhost:%d/tasks?repo=owner/repo-a&status=pending", port), "")
	defer func() { _ = tasksResp.Body.Close() }()
	if tasksResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tasks status = %d", tasksResp.StatusCode)
	}
	var tasks []store.TaskRecord
	if err := json.NewDecoder(tasksResp.Body).Decode(&tasks); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if got := tasks[0].Status; got != store.TaskStatusPending {
		t.Fatalf("task status = %q, want %q", got, store.TaskStatusPending)
	}

	issueResp := getCoordinator(t, client, fmt.Sprintf("http://localhost:%d/issues/owner/repo-a/11/state", port), "")
	defer func() { _ = issueResp.Body.Close() }()
	if issueResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /issues/.../state status = %d", issueResp.StatusCode)
	}
	var issue audit.IssueStateResponse
	if err := json.NewDecoder(issueResp.Body).Decode(&issue); err != nil {
		t.Fatalf("decode issue state: %v", err)
	}
	if issue.Repo != "owner/repo-a" || issue.IssueNum != 11 {
		t.Fatalf("unexpected issue state: %+v", issue)
	}

	watchDone := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://localhost:%d/tasks/watch?repo=owner/repo-a&issue=11", port), nil)
		if err != nil {
			watchDone <- err
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			watchDone <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			watchDone <- fmt.Errorf("watch status = %d", resp.StatusCode)
			return
		}
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				watchDone <- err
				return
			}
			if event["repo"] != "owner/repo-a" || int(event["issue_num"].(float64)) != 11 {
				watchDone <- fmt.Errorf("unexpected watch event: %+v", event)
				return
			}
			watchDone <- nil
			return
		}
		if err := scanner.Err(); err != nil {
			watchDone <- err
			return
		}
		watchDone <- fmt.Errorf("watch stream ended without event")
	}()

	st2, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore for watch publish: %v", err)
	}
	if err := st2.InsertTask(store.TaskRecord{
		ID:        "watch-task",
		Repo:      "owner/repo-a",
		IssueNum:  11,
		AgentName: "dev-agent",
		Status:    store.TaskStatusPending,
	}); err != nil {
		_ = st2.Close()
		t.Fatalf("InsertTask(watch): %v", err)
	}
	if claimed, err := st2.ClaimTask("watch-task", "worker-1"); err != nil {
		_ = st2.Close()
		t.Fatalf("ClaimTask(watch): %v", err)
	} else if !claimed {
		_ = st2.Close()
		t.Fatal("expected ClaimTask(watch) to succeed")
	}
	_ = st2.Close()

	resultResp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/tasks/watch-task/result", port), "", taskResultRequest{
		WorkerID: "worker-1",
		Status:   store.TaskStatusCompleted,
	})
	if resultResp.StatusCode != http.StatusOK {
		defer func() { _ = resultResp.Body.Close() }()
		t.Fatalf("POST /api/v1/tasks/watch-task/result status = %d", resultResp.StatusCode)
	}
	_ = resultResp.Body.Close()

	select {
	case err := <-watchDone:
		if err != nil {
			t.Fatalf("watch request: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch endpoint did not publish an event")
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

func TestCoordinatorAuditEndpointsRequireAuthWhenEnabled(t *testing.T) {
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "secret-token")

	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	seedCoordinatorAuditDB(t, dbPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: time.Second,
			dbPath:       dbPath,
			auth:         true,
		}, nil, ctx)
	}()
	waitForHealth(t, port)

	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := fmt.Sprintf("http://localhost:%d", port)

	for _, path := range []string{
		"/events?repo=owner/repo-a",
		"/tasks?repo=owner/repo-a",
		"/issues/owner/repo-a/11/state",
		"/tasks/watch?repo=owner/repo-a&issue=11",
	} {
		resp := getCoordinator(t, client, baseURL+path, "")
		if resp.StatusCode != http.StatusUnauthorized {
			_ = resp.Body.Close()
			t.Fatalf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusUnauthorized)
		}
		_ = resp.Body.Close()

		authResp := getCoordinator(t, client, baseURL+path, "secret-token")
		if authResp.StatusCode != http.StatusOK {
			_ = authResp.Body.Close()
			t.Fatalf("GET %s with auth status = %d, want %d", path, authResp.StatusCode, http.StatusOK)
		}
		_ = authResp.Body.Close()
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
