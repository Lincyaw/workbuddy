package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/store"
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

	cmd := &cobra.Command{Use: "status"}
	cmd.Flags().String("repo", "", "")
	cmd.Flags().Bool("stuck", false, "")
	cmd.Flags().Bool("json", false, "")

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
