package cmd

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

func TestRestartIssueCommands(t *testing.T) {
	t.Run("canonical issue restart has no deprecation warning", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "restart.db")
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if err := st.UpsertIssueCache(store.IssueCache{Repo: "owner/repo", IssueNum: 173, Labels: `["status:developing"]`, State: "open"}); err != nil {
			t.Fatalf("UpsertIssueCache: %v", err)
		}
		_ = st.Close()

		cmd := &cobra.Command{Use: "restart", RunE: runRestartIssueCmd}
		bindRestartIssueFlags(cmd)
		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		cmd.SetOut(io.Discard)
		_ = cmd.Flags().Set("repo", "owner/repo")
		_ = cmd.Flags().Set("issue", "173")
		_ = cmd.Flags().Set("db-path", dbPath)
		_ = cmd.Flags().Set("force", "true")

		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("runRestartIssueCmd: %v", err)
		}
		if stderr.Len() != 0 {
			t.Fatalf("unexpected stderr: %q", stderr.String())
		}
	})

	t.Run("deprecated admin restart-issue warns on stderr", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "restart.db")
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if err := st.UpsertIssueCache(store.IssueCache{Repo: "owner/repo", IssueNum: 174, Labels: `["status:developing"]`, State: "open"}); err != nil {
			t.Fatalf("UpsertIssueCache: %v", err)
		}
		_ = st.Close()

		cmd := &cobra.Command{Use: "restart-issue", RunE: runAdminRestartIssueCmd}
		bindRestartIssueFlags(cmd)
		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		cmd.SetOut(io.Discard)
		_ = cmd.Flags().Set("repo", "owner/repo")
		_ = cmd.Flags().Set("issue", "174")
		_ = cmd.Flags().Set("db-path", dbPath)
		_ = cmd.Flags().Set("force", "true")

		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("runAdminRestartIssueCmd: %v", err)
		}
		if !strings.Contains(stderr.String(), "`workbuddy admin restart-issue` is deprecated") {
			t.Fatalf("expected deprecation warning, got %q", stderr.String())
		}
	})
}

func TestRunRestartIssueStoreClearsCacheClaimAndLogsEvent(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "restart.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const (
		repo     = "owner/repo"
		issueNum = 173
	)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if err := st.UpsertIssueDependencyState(store.IssueDependencyState{Repo: repo, IssueNum: issueNum, Verdict: store.DependencyVerdictBlocked}); err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}
	if _, err := st.AcquireIssueClaim(repo, issueNum, "coordinator-host-pid-123", time.Hour); err != nil {
		t.Fatalf("AcquireIssueClaim: %v", err)
	}

	result, err := runRestartIssueStore(st, repo, issueNum, "test")
	if err != nil {
		t.Fatalf("runRestartIssueStore: %v", err)
	}
	if !result.CacheCleared || !result.DependencyStateCleared || !result.ClaimCleared || !result.EventLogged {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.ClaimOwner != "coordinator-host-pid-123" {
		t.Fatalf("ClaimOwner = %q", result.ClaimOwner)
	}
	if cache, err := st.QueryIssueCache(repo, issueNum); err != nil || cache != nil {
		t.Fatalf("issue cache after restart = %+v, err=%v", cache, err)
	}
	if dep, err := st.QueryIssueDependencyState(repo, issueNum); err != nil || dep != nil {
		t.Fatalf("issue dependency state after restart = %+v, err=%v", dep, err)
	}
	if claim, err := st.QueryIssueClaim(repo, issueNum); err != nil || claim != nil {
		t.Fatalf("issue claim after restart = %+v, err=%v", claim, err)
	}
	latest, err := st.LatestIssueEvent(repo, issueNum)
	if err != nil {
		t.Fatalf("LatestIssueEvent: %v", err)
	}
	if latest == nil || latest.Type != eventlog.TypeIssueRestarted {
		t.Fatalf("latest event = %+v, want %q", latest, eventlog.TypeIssueRestarted)
	}
}

func TestRestartIssueRedispatchesOnNextPoll(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "redispatch.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const (
		repo     = "owner/repo"
		issueNum = 173
	)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}

	gh := &repoAwareGHReader{issuesByRepo: map[string][]poller.Issue{
		repo: {{Number: issueNum, Title: "Restart me", State: "open", Body: "body", Labels: []string{"workbuddy", "status:developing"}}},
	}}
	workflow := &config.WorkflowConfig{
		Name:    "dev-flow",
		Trigger: config.WorkflowTrigger{IssueLabel: "workbuddy"},
		States: map[string]*config.State{
			"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
		},
	}
	dispatchCh := make(chan statemachine.DispatchRequest, 1)
	sm := statemachine.NewStateMachine(map[string]*config.WorkflowConfig{"dev-flow": workflow}, st, dispatchCh, eventlog.NewEventLogger(st), nil)

	applyPollerEvents(t, sm, runSinglePoll(t, poller.NewPoller(gh, st, repo, time.Hour)))
	select {
	case req := <-dispatchCh:
		t.Fatalf("unexpected dispatch before restart-issue: %+v", req)
	default:
	}

	if _, err := runRestartIssueStore(st, repo, issueNum, "test"); err != nil {
		t.Fatalf("runRestartIssueStore: %v", err)
	}

	applyPollerEvents(t, sm, runSinglePoll(t, poller.NewPoller(gh, st, repo, time.Hour)))
	select {
	case req := <-dispatchCh:
		if req.Repo != repo || req.IssueNum != issueNum || req.AgentName != "dev-agent" {
			t.Fatalf("unexpected dispatch after restart-issue: %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("expected dispatch after restart-issue")
	}
}

func TestRunRestartIssueWithOpts_DryRunHasNoSideEffects(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart-dry-run.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const (
		repo     = "owner/repo"
		issueNum = 173
	)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if err := st.UpsertIssueDependencyState(store.IssueDependencyState{Repo: repo, IssueNum: issueNum, Verdict: store.DependencyVerdictBlocked}); err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}
	if _, err := st.AcquireIssueClaim(repo, issueNum, "coordinator-host-pid-123", time.Hour); err != nil {
		t.Fatalf("AcquireIssueClaim: %v", err)
	}
	_ = st.Close()

	var out bytes.Buffer
	err = runRestartIssueWithOpts(context.Background(), &restartIssueOpts{
		repo:        repo,
		issue:       issueNum,
		dbPath:      dbPath,
		source:      "test",
		dryRun:      true,
		interactive: false,
	}, &out)
	if err != nil {
		t.Fatalf("runRestartIssueWithOpts: %v", err)
	}
	if !strings.Contains(out.String(), "dry-run: owner/repo#173: cache=true dependency_state=true claim=true") {
		t.Fatalf("dry-run output missing preview: %q", out.String())
	}
	if !strings.Contains(out.String(), "event=true") {
		t.Fatalf("dry-run output missing event preview: %q", out.String())
	}

	verify, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = verify.Close() })
	cache, err := verify.QueryIssueCache(repo, issueNum)
	if err != nil || cache == nil {
		t.Fatalf("QueryIssueCache after dry-run = %+v, err=%v", cache, err)
	}
	depState, err := verify.QueryIssueDependencyState(repo, issueNum)
	if err != nil || depState == nil {
		t.Fatalf("QueryIssueDependencyState after dry-run = %+v, err=%v", depState, err)
	}
	claim, err := verify.QueryIssueClaim(repo, issueNum)
	if err != nil || claim == nil {
		t.Fatalf("QueryIssueClaim after dry-run = %+v, err=%v", claim, err)
	}
	events, err := verify.QueryEvents(repo)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events after dry-run = %+v, want none", events)
	}
}

func runSinglePoll(t *testing.T, p *poller.Poller) []poller.ChangeEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Run(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()

	var events []poller.ChangeEvent
	for ev := range p.Events() {
		events = append(events, ev)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("poller run: %v", err)
	}
	return events
}

func applyPollerEvents(t *testing.T, sm *statemachine.StateMachine, events []poller.ChangeEvent) {
	t.Helper()
	for _, ev := range events {
		if ev.Type == poller.EventPollCycleDone {
			sm.ResetDedup()
			continue
		}
		if err := sm.HandleEvent(context.Background(), statemachine.ChangeEvent{
			Type:     ev.Type,
			Repo:     ev.Repo,
			IssueNum: ev.IssueNum,
			Labels:   ev.Labels,
			Detail:   ev.Detail,
			Author:   ev.Author,
		}); err != nil {
			t.Fatalf("HandleEvent(%s): %v", ev.Type, err)
		}
	}
}
