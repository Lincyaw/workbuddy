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
	"github.com/spf13/cobra"
)

type repoAwareGHReader struct {
	mu           sync.Mutex
	issuesByRepo map[string][]poller.Issue
}

func (r *repoAwareGHReader) ListIssues(repo string) ([]poller.Issue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]poller.Issue(nil), r.issuesByRepo[repo]...), nil
}

func (r *repoAwareGHReader) ListPRs(string) ([]poller.PR, error) {
	return nil, nil
}

func (r *repoAwareGHReader) CheckRepoAccess(string) error {
	return nil
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

	var tasksReady bool
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
		if len(tasks) >= 2 {
			tasksReady = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !tasksReady {
		t.Fatal("expected both repos to dispatch tasks")
	}

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

func TestParseRepoRegisterFlagsUsesEnvToken(t *testing.T) {
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "env-token")
	cmd := &cobra.Command{Use: "register"}
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().String("token", "", "")
	cmd.Flags().String("config-dir", "", "")
	cmd.Flags().Duration("timeout", 0, "")
	_ = cmd.Flags().Set("coordinator", "http://localhost:8081")
	_ = cmd.Flags().Set("config-dir", ".github/workbuddy")
	opts, err := parseRepoRegisterFlags(cmd)
	if err != nil {
		t.Fatalf("parseRepoRegisterFlags: %v", err)
	}
	if opts.token != "env-token" {
		t.Fatalf("token = %q, want env-token", opts.token)
	}
	if !strings.HasPrefix(opts.coordinator, "http://localhost") {
		t.Fatalf("unexpected coordinator %q", opts.coordinator)
	}
}
