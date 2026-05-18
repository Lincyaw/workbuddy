package cmd

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
	if result.CycleStateCleared {
		t.Fatalf("CycleStateCleared = true unexpectedly: %+v", result)
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

func TestRunRestartIssueWithOptsClearsCoordinatorInflight(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart-coordinator.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const (
		repo     = "owner/repo"
		issueNum = 218
	)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	_ = st.Close()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got, want := r.URL.Path, "/api/v1/admin/issues/owner/repo/218/clear-inflight"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer shared-secret" {
			t.Fatalf("authorization = %q, want Bearer shared-secret", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"repo":"owner/repo","issue_num":218}`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	err = runRestartIssueWithOpts(context.Background(), &restartIssueOpts{
		repo:        repo,
		issue:       issueNum,
		dbPath:      dbPath,
		coordinator: srv.URL,
		token:       "shared-secret",
		source:      "test",
		force:       true,
		interactive: false,
	}, &out)
	if err != nil {
		t.Fatalf("runRestartIssueWithOpts: %v", err)
	}
	if !called {
		t.Fatal("expected coordinator clear-inflight request")
	}
	if !strings.Contains(out.String(), "inflight=true") {
		t.Fatalf("output missing inflight=true: %q", out.String())
	}
}

func TestRunRestartIssueStoreClearsCycleState(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "restart-cycle.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const (
		repo     = "owner/repo"
		issueNum = 211
	)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if _, err := st.IncrementDevReviewCycleCount(repo, issueNum); err != nil {
		t.Fatalf("IncrementDevReviewCycleCount: %v", err)
	}
	if err := st.MarkIssueCycleCapHit(repo, issueNum); err != nil {
		t.Fatalf("MarkIssueCycleCapHit: %v", err)
	}

	result, err := runRestartIssueStore(st, repo, issueNum, "test")
	if err != nil {
		t.Fatalf("runRestartIssueStore: %v", err)
	}
	if !result.CycleStateCleared {
		t.Fatalf("CycleStateCleared = false: %+v", result)
	}
	state, err := st.QueryIssueCycleState(repo, issueNum)
	if err != nil {
		t.Fatalf("QueryIssueCycleState: %v", err)
	}
	if state != nil {
		t.Fatalf("expected nil cycle state after restart, got %+v", state)
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
	dispatchCh := make(chan statemachine.DispatchRequest, 2)
	sm := statemachine.NewStateMachine(map[string]*config.WorkflowConfig{"dev-flow": workflow}, st, dispatchCh, eventlog.NewEventLogger(st), nil)

	// Reuse a SINGLE poller across both polls so lastResyncAt persists.
	// After the first cycle the resync gate is set; with the interval at
	// 1h the second cycle will NOT emit EventIssueResynced. That makes the
	// second dispatch load-bearing on runRestartIssueStore: it deletes the
	// issue cache row, so the next poll sees wasCached=false and emits
	// EventIssueCreated, which is what re-triggers the dispatch.
	// Interval is intentionally generous (500ms) so the test has time to
	// drain cycle-1 events, run runRestartIssueStore, and prepare to read
	// cycle-2 events before the next tick fires. resyncInterval is set to
	// 1h so the second poll's resync gate is still active — any cycle-2
	// dispatch must come from the cache-cleared EventIssueCreated path,
	// not a re-emitted resync.
	p := poller.NewPoller(gh, st, repo, 500*time.Millisecond)
	p.SetResyncInterval(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx) }()

	// Drain the first poll cycle. REQ-150 (#345) means the first cycle
	// fires EventIssueResynced (issue is already cached with labels) and
	// dispatches dev-agent.
	first := drainUntilCycleDone(t, p.Events(), 2*time.Second)
	if !containsEventType(first, poller.EventIssueResynced) {
		t.Fatalf("first poll missing EventIssueResynced: %+v", first)
	}
	applyPollerEvents(t, sm, first)
	select {
	case req := <-dispatchCh:
		if req.AgentName != "dev-agent" {
			t.Fatalf("expected dev-agent dispatch from REQ-150 resync, got %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("expected REQ-150 resync dispatch on first poll")
	}
	// Mark the resync dispatch complete so the next dispatch path is unblocked.
	sm.MarkAgentCompleted(repo, issueNum, "", "dev-agent", 0, []string{"workbuddy", "status:developing"})

	if _, err := runRestartIssueStore(st, repo, issueNum, "test"); err != nil {
		t.Fatalf("runRestartIssueStore: %v", err)
	}

	// Drain the second poll cycle. Because the resync gate is fresh from
	// cycle 1, EventIssueResynced MUST NOT appear; the only path to a new
	// dispatch is EventIssueCreated triggered by runRestartIssueStore
	// having deleted the cache row. Asserting on both the event kind and
	// the dispatch makes the restart-issue side effect load-bearing.
	second := drainUntilCycleDone(t, p.Events(), 3*time.Second)
	if containsEventType(second, poller.EventIssueResynced) {
		t.Fatalf("second poll unexpectedly fired EventIssueResynced — resync gate failed: %+v", second)
	}
	if !containsEventType(second, poller.EventIssueCreated) {
		t.Fatalf("second poll missing EventIssueCreated — restart-issue did not clear cache: %+v", second)
	}
	applyPollerEvents(t, sm, second)
	select {
	case req := <-dispatchCh:
		if req.Repo != repo || req.IssueNum != issueNum || req.AgentName != "dev-agent" {
			t.Fatalf("unexpected dispatch after restart-issue: %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("expected dispatch after restart-issue")
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("poller run: %v", err)
	}
}

// drainUntilCycleDone reads events from ch until it sees an
// EventPollCycleDone, and returns everything that preceded it (excluding the
// terminator). Used by tests that drive multiple poll cycles through a single
// long-lived Poller.
func drainUntilCycleDone(t *testing.T, ch <-chan poller.ChangeEvent, timeout time.Duration) []poller.ChangeEvent {
	t.Helper()
	deadline := time.After(timeout)
	var events []poller.ChangeEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("poller events channel closed before cycle done; got %+v", events)
			}
			if ev.Type == poller.EventPollCycleDone {
				return events
			}
			events = append(events, ev)
		case <-deadline:
			t.Fatalf("drainUntilCycleDone: timed out after %s; got %+v", timeout, events)
		}
	}
}

func containsEventType(events []poller.ChangeEvent, typ string) bool {
	for _, ev := range events {
		if ev.Type == typ {
			return true
		}
	}
	return false
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
