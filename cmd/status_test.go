package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
	"github.com/spf13/cobra"
)

func TestRunStatusWithOpts_Integration(t *testing.T) {
	st := newStatusTestStore(t)
	fixtureStatusStore(t, st)

	mux := http.NewServeMux()
	audit.NewHTTPHandler(st).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := &statusClient{
		baseURL: srv.URL,
		http:    srv.Client(),
	}

	t.Run("table output", func(t *testing.T) {
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			baseURL: srv.URL,
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}

		got := out.String()
		for _, want := range []string{
			"REPO",
			"ISSUE",
			"STATE",
			"CYCLES",
			"DEPENDENCY",
			"LAST EVENT",
			"STUCK",
			"owner/repo",
			"#1",
			"status:developing",
			"blocked",
			"#2",
			"status:reviewing",
			"override",
			"#3",
			"status:done",
			"ready",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("table output missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "#4") {
			t.Fatalf("closed issue should not be listed:\n%s", got)
		}
	})

	t.Run("stuck filter", func(t *testing.T) {
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			stuck:   true,
			baseURL: srv.URL,
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}

		got := out.String()
		if !strings.Contains(got, "#1") {
			t.Fatalf("expected stuck issue in output:\n%s", got)
		}
		for _, unwanted := range []string{"#2", "#3", "#4"} {
			if strings.Contains(got, unwanted) {
				t.Fatalf("unexpected issue %s in stuck output:\n%s", unwanted, got)
			}
		}
	})

	t.Run("json output", func(t *testing.T) {
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			jsonOut: true,
			baseURL: srv.URL,
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}

		var got statusResponse
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal json output: %v\n%s", err, out.String())
		}
		if got.Repo != "owner/repo" {
			t.Fatalf("repo = %q, want owner/repo", got.Repo)
		}
		if len(got.Issues) != 3 {
			t.Fatalf("expected 3 open issues, got %d: %+v", len(got.Issues), got.Issues)
		}
		if !got.Issues[0].Stuck {
			t.Fatalf("issue #1 should be marked stuck: %+v", got.Issues[0])
		}
	})

	t.Run("tasks output", func(t *testing.T) {
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			tasks:   true,
			baseURL: srv.URL,
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}
		got := out.String()
		for _, want := range []string{"REPO", "AGENT", "pending", "running", "failed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("tasks output missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "completed") {
			t.Fatalf("completed tasks should be filtered out:\n%s", got)
		}
	})

	t.Run("tasks empty", func(t *testing.T) {
		empty := newStatusTestStore(t)
		mux := http.NewServeMux()
		audit.NewHTTPHandler(empty).Register(mux)
		srv := httptest.NewServer(mux)
		defer srv.Close()

		client := &statusClient{baseURL: srv.URL, http: srv.Client()}
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			tasks:   true,
			baseURL: srv.URL,
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}
		if strings.TrimSpace(out.String()) != "No tasks found." {
			t.Fatalf("unexpected empty output: %q", out.String())
		}
	})

	t.Run("tasks json and repo filter", func(t *testing.T) {
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:       "owner/repo",
			tasks:      true,
			taskStatus: store.TaskStatusFailed,
			jsonOut:    true,
			baseURL:    srv.URL,
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}
		var rows []store.TaskRecord
		if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
			t.Fatalf("unmarshal tasks json: %v", err)
		}
		if len(rows) != 1 || rows[0].Status != store.TaskStatusFailed {
			t.Fatalf("unexpected rows: %+v", rows)
		}
		if rows[0].Repo != "owner/repo" {
			t.Fatalf("repo filter not applied: %+v", rows[0])
		}
		if rows[0].Labels == "" {
			t.Fatalf("expected task labels joined from issue_cache: %+v", rows[0])
		}
	})

	t.Run("events output", func(t *testing.T) {
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:      "owner/repo",
			events:    true,
			eventType: "dispatch",
			baseURL:   srv.URL,
			now:       func() time.Time { return time.Now().UTC() },
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}
		got := out.String()
		for _, want := range []string{"TIME", "TYPE", "ISSUE", "PAYLOAD", "dispatch", "#2"} {
			if !strings.Contains(got, want) {
				t.Fatalf("events output missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("events empty", func(t *testing.T) {
		empty := newStatusTestStore(t)
		mux := http.NewServeMux()
		audit.NewHTTPHandler(empty).Register(mux)
		srv := httptest.NewServer(mux)
		defer srv.Close()

		client := &statusClient{baseURL: srv.URL, http: srv.Client()}
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			events:  true,
			baseURL: srv.URL,
			now:     func() time.Time { return time.Now().UTC() },
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}
		if strings.TrimSpace(out.String()) != "No events found." {
			t.Fatalf("unexpected empty events output: %q", out.String())
		}
	})

	t.Run("events json and since filter", func(t *testing.T) {
		now := time.Now().UTC()
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			events:  true,
			since:   "30m",
			jsonOut: true,
			baseURL: srv.URL,
			now:     func() time.Time { return now },
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}
		var rows []statusEventRow
		if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
			t.Fatalf("unmarshal events json: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("expected recent events")
		}
		for _, row := range rows {
			if row.IssueNum != 2 {
				t.Fatalf("event not filtered by since: %+v", row)
			}
		}
	})
}

func TestRunStatusWithOpts_SkipsMissingIssueState(t *testing.T) {
	st := newStatusTestStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 1,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	insertEventAt(t, st, "owner/repo", 1, time.Now().UTC().Add(-5*time.Minute))
	insertEventAt(t, st, "owner/repo", 999, time.Now().UTC().Add(-2*time.Minute))

	mux := http.NewServeMux()
	audit.NewHTTPHandler(st).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &statusClient{baseURL: srv.URL, http: srv.Client()}
	var out bytes.Buffer
	err := runStatusWithOpts(context.Background(), &statusOpts{
		repo:    "owner/repo",
		baseURL: srv.URL,
	}, client, &out)
	if err != nil {
		t.Fatalf("runStatusWithOpts: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "#1") {
		t.Fatalf("expected live issue in output:\n%s", got)
	}
	if strings.Contains(got, "#999") {
		t.Fatalf("unexpected stale issue in output:\n%s", got)
	}
}

func newStatusTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "status.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func fixtureStatusStore(t *testing.T, st *store.Store) {
	t.Helper()

	for _, issue := range []struct {
		num    int
		labels string
		state  string
	}{
		{num: 1, labels: `["workbuddy","status:developing"]`, state: "open"},
		{num: 2, labels: `["workbuddy","status:reviewing"]`, state: "open"},
		{num: 3, labels: `["workbuddy","status:done"]`, state: "open"},
		{num: 4, labels: `["workbuddy","status:developing"]`, state: "closed"},
	} {
		if err := st.UpsertIssueCache(store.IssueCache{
			Repo:     "owner/repo",
			IssueNum: issue.num,
			Labels:   issue.labels,
			State:    issue.state,
		}); err != nil {
			t.Fatalf("UpsertIssueCache(%d): %v", issue.num, err)
		}
	}

	for _, transition := range []store.TransitionCount{
		{Repo: "owner/repo", IssueNum: 1, FromState: "reviewing", ToState: "developing", Count: 2},
		{Repo: "owner/repo", IssueNum: 2, FromState: "developing", ToState: "reviewing", Count: 1},
		{Repo: "owner/repo", IssueNum: 3, FromState: "reviewing", ToState: "done", Count: 1},
	} {
		if _, err := st.DB().Exec(
			`INSERT INTO transition_counts (repo, issue_num, from_state, to_state, count) VALUES (?, ?, ?, ?, ?)`,
			transition.Repo, transition.IssueNum, transition.FromState, transition.ToState, transition.Count,
		); err != nil {
			t.Fatalf("insert transition count %+v: %v", transition, err)
		}
	}

	for _, depState := range []store.IssueDependencyState{
		{Repo: "owner/repo", IssueNum: 1, Verdict: store.DependencyVerdictBlocked},
		{Repo: "owner/repo", IssueNum: 2, Verdict: store.DependencyVerdictOverride},
	} {
		if err := st.UpsertIssueDependencyState(depState); err != nil {
			t.Fatalf("UpsertIssueDependencyState(%d): %v", depState.IssueNum, err)
		}
	}

	now := time.Now().UTC()
	insertEventAt(t, st, "owner/repo", 1, now.Add(-2*time.Hour))
	insertEventAt(t, st, "owner/repo", 2, now.Add(-10*time.Minute))
	insertEventAt(t, st, "owner/repo", 3, now.Add(-3*time.Hour))
	insertEventAt(t, st, "owner/repo", 4, now.Add(-2*time.Hour))
	dispatchID, err := st.InsertEvent(store.Event{
		Type:     "dispatch",
		Repo:     "owner/repo",
		IssueNum: 2,
		Payload:  `{"agent":"dev-agent","message":"dispatch payload for testing"}`,
	})
	if err != nil {
		t.Fatalf("InsertEvent(dispatch): %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE events SET ts = ? WHERE id = ?`, now.Add(-5*time.Minute).Format("2006-01-02 15:04:05"), dispatchID); err != nil {
		t.Fatalf("UPDATE dispatch event ts: %v", err)
	}
	for _, task := range []store.TaskRecord{
		{ID: "task-pending", Repo: "owner/repo", IssueNum: 1, AgentName: "dev-agent", Status: store.TaskStatusPending},
		{ID: "task-running", Repo: "owner/repo", IssueNum: 2, AgentName: "review-agent", WorkerID: "worker-1", Status: store.TaskStatusRunning},
		{ID: "task-failed", Repo: "owner/repo", IssueNum: 3, AgentName: "dev-agent", WorkerID: "worker-2", Status: store.TaskStatusFailed},
		{ID: "task-completed", Repo: "owner/repo", IssueNum: 4, AgentName: "dev-agent", Status: store.TaskStatusCompleted},
		{ID: "task-other-repo", Repo: "other/repo", IssueNum: 99, AgentName: "dev-agent", Status: store.TaskStatusFailed},
	} {
		if err := st.InsertTask(task); err != nil {
			t.Fatalf("InsertTask(%s): %v", task.ID, err)
		}
	}
}

func insertEventAt(t *testing.T, st *store.Store, repo string, issueNum int, ts time.Time) {
	t.Helper()
	id, err := st.InsertEvent(store.Event{
		Type:     "transition",
		Repo:     repo,
		IssueNum: issueNum,
		Payload:  `{"state":"fixture"}`,
	})
	if err != nil {
		t.Fatalf("InsertEvent(%d): %v", issueNum, err)
	}
	if _, err := st.DB().Exec(`UPDATE events SET ts = ? WHERE id = ?`, ts.Format("2006-01-02 15:04:05"), id); err != nil {
		t.Fatalf("UPDATE events ts issue %d: %v", issueNum, err)
	}
}

func TestParseStatusFlags_UsesConfigDefaults(t *testing.T) {
	isolateStatusConfigHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".github", "workbuddy", "config.yaml"), "repo: cfg/repo\nport: 9123\npoll_interval: 30s\n")

	prevWD, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("Abs(.): %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})

	cmd := newStatusFlagCommand()

	opts, err := parseStatusFlags(cmd)
	if err != nil {
		t.Fatalf("parseStatusFlags: %v", err)
	}
	if opts.repo != "cfg/repo" {
		t.Fatalf("repo = %q, want cfg/repo", opts.repo)
	}
	if opts.baseURL != "http://127.0.0.1:9123" {
		t.Fatalf("baseURL = %q", opts.baseURL)
	}
}

func TestParseStatusFlags_UsesExplicitRepoWithoutConfig(t *testing.T) {
	isolateStatusConfigHome(t)
	dir := t.TempDir()

	prevWD, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("Abs(.): %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})

	cmd := newStatusFlagCommand()
	if err := cmd.Flags().Set("repo", "explicit/repo"); err != nil {
		t.Fatalf("Set(repo): %v", err)
	}

	opts, err := parseStatusFlags(cmd)
	if err != nil {
		t.Fatalf("parseStatusFlags: %v", err)
	}
	if opts.repo != "explicit/repo" {
		t.Fatalf("repo = %q, want explicit/repo", opts.repo)
	}
	if opts.baseURL != "http://127.0.0.1:8080" {
		t.Fatalf("baseURL = %q", opts.baseURL)
	}
}

func TestParseStatusFlags_UsesManagedCoordinatorDeployment(t *testing.T) {
	dir := t.TempDir()
	configHome := filepath.Join(t.TempDir(), ".config")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", configHome)

	writeFile(t, filepath.Join(dir, ".github", "workbuddy", "config.yaml"), "repo: owner/repo\nport: 8090\npoll_interval: 30s\n")
	envFile := filepath.Join(configHome, "workbuddy", "workbuddy.env")
	writeFile(t, envFile, "WORKBUDDY_AUTH_TOKEN=file-token\n")

	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "workbuddy-coordinator",
		Scope:            "user",
		BinaryPath:       "/tmp/workbuddy",
		WorkingDirectory: dir,
		Command:          []string{"coordinator", "--listen", "127.0.0.1:8081", "--config-dir", ".github/workbuddy", "--auth"},
		Systemd: &deploymentSystemd{
			EnvironmentFiles: []string{envFile},
		},
	}
	manifestPath := filepath.Join(configHome, "workbuddy", "deployments", "workbuddy-coordinator.json")
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("writeDeploymentManifest: %v", err)
	}

	prevWD, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("Abs(.): %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})

	cmd := newStatusFlagCommand()
	opts, err := parseStatusFlags(cmd)
	if err != nil {
		t.Fatalf("parseStatusFlags: %v", err)
	}
	if opts.baseURL != "http://127.0.0.1:8081" {
		t.Fatalf("baseURL = %q, want http://127.0.0.1:8081", opts.baseURL)
	}
	if opts.token != "file-token" {
		t.Fatalf("token = %q, want file-token", opts.token)
	}
}

func TestParseStatusFlags_MutualExclusion(t *testing.T) {
	cmd := newStatusFlagCommand()
	_ = cmd.Flags().Set("repo", "owner/repo")
	_ = cmd.Flags().Set("tasks", "true")
	_ = cmd.Flags().Set("events", "true")

	_, err := parseStatusFlags(cmd)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual exclusion error, got %v", err)
	}
}

func TestRunStatusWithOpts_Watch(t *testing.T) {
	t.Run("receives completion event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(t, tasknotify.TaskEvent{
				Repo:       "owner/repo",
				IssueNum:   7,
				AgentName:  "dev-agent",
				Status:     store.TaskStatusCompleted,
				ExitCode:   0,
				DurationMS: int64((12 * time.Second).Milliseconds()),
			}))
		}))
		defer srv.Close()

		client := &statusClient{baseURL: srv.URL, http: srv.Client()}
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			watch:   true,
			timeout: time.Second,
			baseURL: srv.URL,
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}
		got := out.String()
		for _, want := range []string{"Waiting for task completion...", "ISSUE", "#7", "completed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("watch output missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("issue filter propagated", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("issue"); got != "9" {
				t.Fatalf("issue query = %q, want 9", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(t, tasknotify.TaskEvent{
				Repo:     "owner/repo",
				IssueNum: 9, AgentName: "dev-agent", Status: store.TaskStatusFailed, ExitCode: 1,
			}))
		}))
		defer srv.Close()

		client := &statusClient{baseURL: srv.URL, http: srv.Client()}
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			watch:   true,
			issue:   9,
			timeout: time.Second,
			baseURL: srv.URL,
		}, client, io.Discard)
		var exitErr *cliExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit code 1, got %v", err)
		}
	})

	t.Run("watch timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer srv.Close()

		client := &statusClient{baseURL: srv.URL, http: srv.Client()}
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			watch:   true,
			timeout: 50 * time.Millisecond,
			baseURL: srv.URL,
		}, client, &out)
		var exitErr *cliExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Fatalf("expected exit code 3, got %v", err)
		}
		if !strings.Contains(out.String(), "No task completed within timeout") {
			t.Fatalf("timeout output = %q", out.String())
		}
	})

	t.Run("server unavailable", func(t *testing.T) {
		client := &statusClient{
			baseURL: "http://127.0.0.1:1",
			http:    &http.Client{Timeout: 50 * time.Millisecond},
		}
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			watch:   true,
			timeout: time.Second,
			baseURL: client.baseURL,
		}, client, io.Discard)
		var exitErr *cliExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || exitErr.Error() != "Cannot connect to workbuddy server" {
			t.Fatalf("unexpected error: %#v", err)
		}
	})
}

func TestStatusClient_AppliesAuthHeader(t *testing.T) {
	t.Run("json requests", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if auth := r.Header.Get("Authorization"); auth != "Bearer file-token" {
				t.Fatalf("authorization = %q", auth)
			}
			_, _ = fmt.Fprint(w, `{"events":[]}`)
		}))
		defer srv.Close()

		client := &statusClient{baseURL: srv.URL, token: "file-token", http: srv.Client()}
		var out bytes.Buffer
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			events:  true,
			baseURL: srv.URL,
		}, client, &out)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}
		if strings.TrimSpace(out.String()) != "No events found." {
			t.Fatalf("unexpected output: %q", out.String())
		}
	})

	t.Run("watch requests", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if auth := r.Header.Get("Authorization"); auth != "Bearer file-token" {
				t.Fatalf("authorization = %q", auth)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(t, tasknotify.TaskEvent{
				Repo:       "owner/repo",
				IssueNum:   11,
				AgentName:  "dev-agent",
				Status:     store.TaskStatusCompleted,
				ExitCode:   0,
				DurationMS: int64(time.Second.Milliseconds()),
			}))
		}))
		defer srv.Close()

		client := &statusClient{baseURL: srv.URL, token: "file-token", http: srv.Client()}
		err := runStatusWithOpts(context.Background(), &statusOpts{
			repo:    "owner/repo",
			watch:   true,
			timeout: time.Second,
			baseURL: srv.URL,
		}, client, io.Discard)
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}
	})
}

func TestStatusClient_IssueStateUsesSegmentedRepoPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.RequestURI, "/issues/owner/repo%20name/7/state"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		_, _ = fmt.Fprint(w, `{"repo":"owner/repo name","issue_num":7,"issue_state":"open","current_state":"status:developing","cycle_count":1,"dependency_verdict":"ready"}`)
	}))
	defer srv.Close()

	client := &statusClient{baseURL: srv.URL, http: srv.Client()}
	resp, err := client.issueState(context.Background(), "owner/repo name", 7)
	if err != nil {
		t.Fatalf("issueState: %v", err)
	}
	if resp.Repo != "owner/repo name" || resp.IssueNum != 7 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func isolateStatusConfigHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), ".config"))
}

func newStatusFlagCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "status"}
	cmd.Flags().String("repo", "", "")
	cmd.Flags().Bool("stuck", false, "")
	cmd.Flags().Bool("tasks", false, "")
	cmd.Flags().Bool("events", false, "")
	cmd.Flags().Bool("watch", false, "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().String("status", "", "")
	cmd.Flags().String("type", "", "")
	cmd.Flags().String("since", "", "")
	cmd.Flags().Int("issue", 0, "")
	cmd.Flags().Duration("timeout", defaultWatchTimeout, "")
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().StringP("token", "t", "", "")
	cmd.Flags().Bool("repos", false, "")
	return cmd
}

func TestParseStatusFlags_Coordinator(t *testing.T) {
	cmd := newStatusFlagCommand()
	_ = cmd.Flags().Set("coordinator", "http://127.0.0.1:8091")

	opts, err := parseStatusFlags(cmd)
	if err != nil {
		t.Fatalf("parseStatusFlags: %v", err)
	}
	if opts.coordinator != "http://127.0.0.1:8091" {
		t.Fatalf("coordinator = %q", opts.coordinator)
	}
	if opts.repos {
		t.Fatal("repos should be false")
	}
}

func TestParseStatusFlags_CoordinatorRepos(t *testing.T) {
	cmd := newStatusFlagCommand()
	_ = cmd.Flags().Set("coordinator", "http://127.0.0.1:8091")
	_ = cmd.Flags().Set("repos", "true")
	_ = cmd.Flags().Set("json", "true")

	opts, err := parseStatusFlags(cmd)
	if err != nil {
		t.Fatalf("parseStatusFlags: %v", err)
	}
	if !opts.repos {
		t.Fatal("repos should be true")
	}
	if !opts.jsonOut {
		t.Fatal("jsonOut should be true")
	}
}

func TestParseStatusFlags_CoordinatorTokenFromEnv(t *testing.T) {
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "env-token")

	cmd := newStatusFlagCommand()
	_ = cmd.Flags().Set("coordinator", "http://127.0.0.1:8091")

	opts, err := parseStatusFlags(cmd)
	if err != nil {
		t.Fatalf("parseStatusFlags: %v", err)
	}
	if opts.token != "env-token" {
		t.Fatalf("token = %q", opts.token)
	}
}

func TestParseStatusFlags_CoordinatorMutualExclusion(t *testing.T) {
	for _, flag := range []string{"stuck", "tasks", "events", "watch"} {
		cmd := newStatusFlagCommand()
		_ = cmd.Flags().Set("coordinator", "http://127.0.0.1:8091")
		_ = cmd.Flags().Set(flag, "true")

		_, err := parseStatusFlags(cmd)
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("expected mutual exclusion error for --%s, got %v", flag, err)
		}
	}
}

func TestParseStatusFlags_ReposRequiresCoordinator(t *testing.T) {
	cmd := newStatusFlagCommand()
	_ = cmd.Flags().Set("repos", "true")

	_, err := parseStatusFlags(cmd)
	if err == nil || !strings.Contains(err.Error(), "--repos requires --coordinator") {
		t.Fatalf("expected --repos requires --coordinator error, got %v", err)
	}
}

func TestRunStatusCoordinator_Health(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Fatalf("authorization = %q", auth)
		}
		_, _ = fmt.Fprint(w, `{"status":"ok","repos":3}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runStatusCoordinator(context.Background(), &statusOpts{
		coordinator: srv.URL,
		token:       "test-token",
	}, &out)
	if err != nil {
		t.Fatalf("runStatusCoordinator: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "status: ok") {
		t.Fatalf("expected status: ok, got %q", got)
	}
	if !strings.Contains(got, "repos: 3") {
		t.Fatalf("expected repos: 3, got %q", got)
	}
}

func TestRunStatusCoordinator_HealthJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"ok","repos":2}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runStatusCoordinator(context.Background(), &statusOpts{
		coordinator: srv.URL,
		jsonOut:     true,
	}, &out)
	if err != nil {
		t.Fatalf("runStatusCoordinator: %v", err)
	}
	var health map[string]any
	if err := json.Unmarshal(out.Bytes(), &health); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if health["status"] != "ok" {
		t.Fatalf("status = %v", health["status"])
	}
}

func TestRunStatusCoordinator_Repos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, mustJSON(t, []repoStatusResponse{
			{Repo: "owner/a", Environment: "prod", Status: "active", PollerStatus: "running"},
			{Repo: "owner/b", Environment: "dev", Status: "active", PollerStatus: "stopped"},
		}))
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runStatusCoordinator(context.Background(), &statusOpts{
		coordinator: srv.URL,
		repos:       true,
	}, &out)
	if err != nil {
		t.Fatalf("runStatusCoordinator: %v", err)
	}
	got := out.String()
	for _, want := range []string{"owner/a", "owner/b", "prod", "dev", "running", "stopped"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunStatusCoordinator_ReposJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[{"repo":"owner/a","status":"active","poller_status":"running"}]`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runStatusCoordinator(context.Background(), &statusOpts{
		coordinator: srv.URL,
		repos:       true,
		jsonOut:     true,
	}, &out)
	if err != nil {
		t.Fatalf("runStatusCoordinator: %v", err)
	}
	var repos []repoStatusResponse
	if err := json.Unmarshal(out.Bytes(), &repos); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(repos) != 1 || repos[0].Repo != "owner/a" {
		t.Fatalf("unexpected repos: %+v", repos)
	}
}

func TestRunStatusCoordinator_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runStatusCoordinator(context.Background(), &statusOpts{
		coordinator: srv.URL,
	}, &out)
	var exitErr *cliExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit error with code 1, got %v", err)
	}
	if !strings.Contains(exitErr.Error(), "401") {
		t.Fatalf("expected 401 in error, got %q", exitErr.Error())
	}
}

func TestRenderRepoStatusTable_Empty(t *testing.T) {
	var out bytes.Buffer
	renderRepoStatusTable(&out, nil)
	if strings.TrimSpace(out.String()) != "No repos found." {
		t.Fatalf("unexpected empty output: %q", out.String())
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}
