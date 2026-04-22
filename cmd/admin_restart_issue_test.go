package cmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

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
