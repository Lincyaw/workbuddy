package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type repoAwareGHReader struct {
	mu           sync.Mutex
	issuesByRepo map[string][]poller.Issue
	accessErrs   map[string]error
}

func (r *repoAwareGHReader) ListIssues(repo string) ([]poller.Issue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]poller.Issue(nil), r.issuesByRepo[repo]...), nil
}

func (r *repoAwareGHReader) ListPRs(string) ([]poller.PR, error) {
	return nil, nil
}

func (r *repoAwareGHReader) CheckRepoAccess(repo string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accessErrs == nil {
		return nil
	}
	return r.accessErrs[strings.TrimSpace(repo)]
}

func (r *repoAwareGHReader) ReadIssue(repo string, issueNum int) (poller.IssueDetails, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, issue := range r.issuesByRepo[repo] {
		if issue.Number == issueNum {
			return poller.IssueDetails{
				Number: issue.Number,
				State:  issue.State,
				Body:   issue.Body,
				Labels: append([]string(nil), issue.Labels...),
			}, nil
		}
	}
	return poller.IssueDetails{Number: issueNum, State: "open"}, nil
}

func (r *repoAwareGHReader) ReadIssueLabels(repo string, issueNum int) ([]string, error) {
	details, err := r.ReadIssue(repo, issueNum)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), details.Labels...), nil
}

func setupNamedConfigDir(t *testing.T, repo, agentName, workflowName string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), fmt.Sprintf("repo: %s\nenvironment: test\npoll_interval: 1s\nport: 0\n", repo))
	writeFile(t, filepath.Join(dir, "agents", agentName+".md"), fmt.Sprintf(`---
name: %s
description: %s
triggers:
  - label: "status:developing"
role: dev
runtime: claude-code
command: echo "hello"
timeout: 30s
---
# Agent
`, agentName, agentName))
	writeFile(t, filepath.Join(dir, "workflows", workflowName+".md"), fmt.Sprintf(`---
name: %s
description: %s
trigger:
  issue_label: "workbuddy"
max_retries: 3
---
# Workflow

`+"```yaml\nstates:\n  triage:\n    enter_label: \"status:triage\"\n    transitions:\n      - to: developing\n        when: 'labeled \"status:developing\"'\n  developing:\n    enter_label: \"status:developing\"\n    agent: %s\n    transitions:\n      - to: done\n        when: 'labeled \"status:done\"'\n  done:\n    enter_label: \"status:done\"\n```\n", workflowName, workflowName, agentName))
	return dir
}

func postCoordinatorJSON(t *testing.T, client *http.Client, url, token string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	return resp
}

func deleteCoordinator(t *testing.T, client *http.Client, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("delete %s: %v", url, err)
	}
	return resp
}

func getCoordinator(t *testing.T, client *http.Client, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new get request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return resp
}

func mustRegistrationRequest(t *testing.T, configDir string) repoRegisterRequest {
	t.Helper()
	cfg, _, err := config.LoadConfig(configDir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	payload := buildRepoRegistrationPayload(cfg)
	return repoRegisterRequest{
		Repo:        payload.Repo,
		Environment: payload.Environment,
		Agents:      payload.Agents,
		Workflows:   payload.Workflows,
	}
}

func waitForPendingTasks(t *testing.T, dbPath string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		tasks, err := st.QueryTasks(store.TaskStatusPending)
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected at least %d pending tasks", want)
}

func waitForTaskCount(t *testing.T, dbPath, status string, want int) []store.TaskRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		tasks, err := st.QueryTasks(status)
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) >= want {
			return tasks
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected at least %d tasks with status %s", want, status)
	return nil
}

func waitForRepoStatus(t *testing.T, client *http.Client, url, repo, wantStatus string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := getCoordinator(t, client, url, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list repos status = %d", resp.StatusCode)
		}
		var repos []repoStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
			_ = resp.Body.Close()
			t.Fatalf("decode repos: %v", err)
		}
		_ = resp.Body.Close()
		for _, status := range repos {
			if status.Repo == repo && status.PollerStatus == wantStatus {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("repo %s did not reach poller status %s", repo, wantStatus)
}

func TestCoordinatorMultiRepoRegistrationIsolationAndDeregister(t *testing.T) {
	repoA := "owner/repo-a"
	repoB := "owner/repo-b"
	configA := setupNamedConfigDir(t, repoA, "dev-agent-a", "workflow-a")
	configB := setupNamedConfigDir(t, repoB, "dev-agent-b", "workflow-b")
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &repoAwareGHReader{
		issuesByRepo: map[string][]poller.Issue{
			repoA: {{Number: 11, Title: "A", State: "open", Body: "body-a", Labels: []string{"workbuddy", "status:developing"}}},
			repoB: {{Number: 22, Title: "B", State: "open", Body: "body-b", Labels: []string{"workbuddy", "status:developing"}}},
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
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port), "", mustRegistrationRequest(t, configA))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register repo A status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp = postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port), "", mustRegistrationRequest(t, configB))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register repo B status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	waitForPendingTasks(t, dbPath, 2)

	reRegister := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port), "", mustRegistrationRequest(t, configA))
	if reRegister.StatusCode != http.StatusOK {
		t.Fatalf("re-register repo A status = %d", reRegister.StatusCode)
	}
	_ = reRegister.Body.Close()

	listResp := getCoordinator(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos", port), "")
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list repos status = %d", listResp.StatusCode)
	}
	var repos []repoStatusResponse
	if err := json.NewDecoder(listResp.Body).Decode(&repos); err != nil {
		t.Fatalf("decode repos: %v", err)
	}
	_ = listResp.Body.Close()
	if len(repos) != 2 {
		t.Fatalf("repos = %d, want 2", len(repos))
	}

	workerA := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/workers/register", port), "", workerRegisterRequest{
		WorkerID: "worker-a",
		Repo:     repoA,
		Repos:    []string{repoA},
		Roles:    []string{"dev"},
		Hostname: "host-a",
	})
	if workerA.StatusCode != http.StatusCreated {
		t.Fatalf("register worker A status = %d", workerA.StatusCode)
	}
	_ = workerA.Body.Close()

	pollAResp, err := client.Get(fmt.Sprintf("http://localhost:%d/api/v1/tasks/poll?worker_id=worker-a&timeout=100ms", port))
	if err != nil {
		t.Fatalf("poll worker A: %v", err)
	}
	if pollAResp.StatusCode != http.StatusOK {
		t.Fatalf("poll worker A status = %d", pollAResp.StatusCode)
	}
	var taskA taskPollResponse
	if err := json.NewDecoder(pollAResp.Body).Decode(&taskA); err != nil {
		t.Fatalf("decode task A: %v", err)
	}
	_ = pollAResp.Body.Close()
	if taskA.Repo != repoA || taskA.AgentName != "dev-agent-a" {
		t.Fatalf("worker A received %+v", taskA)
	}

	rejectWorker := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/workers/register", port), "", workerRegisterRequest{
		WorkerID: "worker-c",
		Repo:     "owner/repo-c",
		Repos:    []string{"owner/repo-c"},
		Roles:    []string{"dev"},
		Hostname: "host-c",
	})
	if rejectWorker.StatusCode != http.StatusBadRequest {
		t.Fatalf("unregistered repo worker status = %d, want 400", rejectWorker.StatusCode)
	}
	_ = rejectWorker.Body.Close()

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(store.TaskRecord{
		ID:        "manual-a",
		Repo:      repoA,
		IssueNum:  99,
		AgentName: "dev-agent-a",
		Role:      "dev",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	deleteResp := deleteCoordinator(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/%s", port, repoA), "")
	if deleteResp.StatusCode != http.StatusOK {
		t.Fatalf("delete repo A status = %d", deleteResp.StatusCode)
	}
	_ = deleteResp.Body.Close()

	st, err = store.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.GetTask("manual-a")
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.Status != store.TaskStatusFailed {
		t.Fatalf("manual task after deregister = %+v, want failed", task)
	}

	listResp = getCoordinator(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos", port), "")
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list repos after delete status = %d", listResp.StatusCode)
	}
	repos = nil
	if err := json.NewDecoder(listResp.Body).Decode(&repos); err != nil {
		t.Fatalf("decode repos after delete: %v", err)
	}
	_ = listResp.Body.Close()
	if len(repos) != 1 || repos[0].Repo != repoB {
		t.Fatalf("repos after delete = %+v", repos)
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

func TestCoordinatorRestartRedispatchesOrphanedActiveState(t *testing.T) {
	repo := "owner/restart-repo"
	configDir := setupNamedConfigDir(t, repo, "dev-agent-restart", "workflow-restart")
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &repoAwareGHReader{
		issuesByRepo: map[string][]poller.Issue{
			repo: {{
				Number: 37,
				Title:  "Restart me",
				State:  "open",
				Body:   "body",
				Labels: []string{"workbuddy", "status:developing"},
			}},
		},
	}

	port1 := getFreePort(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	errCh1 := make(chan error, 1)
	go func() {
		errCh1 <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port1,
			pollInterval: 50 * time.Millisecond,
			dbPath:       dbPath,
		}, gh, ctx1)
	}()
	waitForHealth(t, port1)

	client := &http.Client{Timeout: 5 * time.Second}
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port1), "", mustRegistrationRequest(t, configDir))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register repo status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	waitForPendingTasks(t, dbPath, 1)

	workerResp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/workers/register", port1), "", workerRegisterRequest{
		WorkerID: "worker-restart",
		Repo:     repo,
		Repos:    []string{repo},
		Roles:    []string{"dev"},
		Hostname: "host-restart",
	})
	if workerResp.StatusCode != http.StatusCreated {
		t.Fatalf("register worker status = %d", workerResp.StatusCode)
	}
	_ = workerResp.Body.Close()

	pollResp, err := client.Get(fmt.Sprintf("http://localhost:%d/api/v1/tasks/poll?worker_id=worker-restart&timeout=100ms", port1))
	if err != nil {
		t.Fatalf("poll worker: %v", err)
	}
	if pollResp.StatusCode != http.StatusOK {
		t.Fatalf("poll worker status = %d", pollResp.StatusCode)
	}
	var task taskPollResponse
	if err := json.NewDecoder(pollResp.Body).Decode(&task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	_ = pollResp.Body.Close()
	if task.IssueNum != 37 || task.AgentName != "dev-agent-restart" {
		t.Fatalf("unexpected task after first dispatch: %+v", task)
	}

	cancel1()
	select {
	case err := <-errCh1:
		if err != nil {
			t.Fatalf("first coordinator: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first coordinator did not exit")
	}

	port2 := getFreePort(t)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	errCh2 := make(chan error, 1)
	go func() {
		errCh2 <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port2,
			pollInterval: 50 * time.Millisecond,
			dbPath:       dbPath,
		}, gh, ctx2)
	}()
	waitForHealth(t, port2)

	failed := waitForTaskCount(t, dbPath, store.TaskStatusFailed, 1)
	if failed[0].IssueNum != 37 {
		t.Fatalf("failed task issue = %d, want 37", failed[0].IssueNum)
	}

	pending := waitForTaskCount(t, dbPath, store.TaskStatusPending, 1)
	foundRedispatch := false
	for _, queued := range pending {
		if queued.IssueNum == 37 && queued.AgentName == "dev-agent-restart" {
			foundRedispatch = true
			break
		}
	}
	if !foundRedispatch {
		t.Fatalf("pending tasks after restart = %+v, want redispatched task for issue 37", pending)
	}

	cancel2()
	select {
	case err := <-errCh2:
		if err != nil {
			t.Fatalf("second coordinator: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second coordinator did not exit")
	}
}

func TestCoordinatorRepoRegistrationAuthRequired(t *testing.T) {
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "secret-token")
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	configDir := setupNamedConfigDir(t, "owner/auth-repo", "dev-agent-auth", "workflow-auth")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: 50 * time.Millisecond,
			dbPath:       dbPath,
			auth:         true,
		}, &repoAwareGHReader{}, ctx)
	}()
	waitForHealth(t, port)

	client := &http.Client{Timeout: 5 * time.Second}
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port), "", mustRegistrationRequest(t, configDir))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated register status = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()

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

func TestCoordinatorRegisterRepoRollsBackOnStartupFailure(t *testing.T) {
	repo := "owner/failing-repo"
	configDir := setupNamedConfigDir(t, repo, "dev-agent-fail", "workflow-fail")
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
		}, &repoAwareGHReader{
			accessErrs: map[string]error{
				repo: fmt.Errorf("no access"),
			},
		}, ctx)
	}()
	waitForHealth(t, port)

	client := &http.Client{Timeout: 5 * time.Second}
	resp := postCoordinatorJSON(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos/register", port), "", mustRegistrationRequest(t, configDir))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("register failing repo status = %d, want 500", resp.StatusCode)
	}
	_ = resp.Body.Close()

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.GetRepoRegistration(repo)
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatalf("repo registration persisted after startup failure: %+v", rec)
	}

	listResp := getCoordinator(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos", port), "")
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list repos status = %d", listResp.StatusCode)
	}
	var repos []repoStatusResponse
	if err := json.NewDecoder(listResp.Body).Decode(&repos); err != nil {
		t.Fatalf("decode repos: %v", err)
	}
	_ = listResp.Body.Close()
	if len(repos) != 0 {
		t.Fatalf("repos after failed registration = %+v, want empty", repos)
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

func TestRepoRegisterCLI(t *testing.T) {
	configDir := setupNamedConfigDir(t, "owner/cli-repo", "dev-agent-cli", "workflow-cli")

	var got repoRegisterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/register" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	payload, err := runRepoRegister(context.Background(), &repoRegisterOpts{
		coordinator: srv.URL,
		configDir:   configDir,
		timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("runRepoRegister: %v", err)
	}
	if payload.Repo != "owner/cli-repo" {
		t.Fatalf("payload repo = %q", payload.Repo)
	}
	if got.Repo != "owner/cli-repo" || len(got.Agents) != 1 || len(got.Workflows) != 1 {
		t.Fatalf("unexpected request payload: %+v", got)
	}
}

func TestRepoRegisterCLIStartsPollerOnCoordinator(t *testing.T) {
	repo := "owner/cli-live-repo"
	configDir := setupNamedConfigDir(t, repo, "dev-agent-cli-live", "workflow-cli-live")
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	gh := &repoAwareGHReader{
		issuesByRepo: map[string][]poller.Issue{
			repo: {{Number: 31, Title: "CLI", State: "open", Body: "body-cli", Labels: []string{"workbuddy", "status:developing"}}},
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

	payload, err := runRepoRegister(context.Background(), &repoRegisterOpts{
		coordinator: fmt.Sprintf("http://localhost:%d", port),
		configDir:   configDir,
		timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("runRepoRegister: %v", err)
	}
	if payload.Repo != repo {
		t.Fatalf("payload repo = %q, want %q", payload.Repo, repo)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	waitForRepoStatus(t, client, fmt.Sprintf("http://localhost:%d/api/v1/repos", port), repo, "running")
	waitForPendingTasks(t, dbPath, 1)

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.GetRepoRegistration(repo)
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected repo registration to be stored")
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
