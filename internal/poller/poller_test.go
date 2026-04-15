package poller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// ---------------------------------------------------------------------------
// Mock GHReader
// ---------------------------------------------------------------------------

type mockGHReader struct {
	issues      []Issue
	prs         []PR
	issuesErr   error
	prsErr      error
	accessErr   error
	listCalls   int
	prListCalls int
}

func (m *mockGHReader) ListIssues(_ string) ([]Issue, error) {
	m.listCalls++
	return m.issues, m.issuesErr
}

func (m *mockGHReader) ListPRs(_ string) ([]PR, error) {
	m.prListCalls++
	return m.prs, m.prsErr
}

func (m *mockGHReader) CheckRepoAccess(_ string) error {
	return m.accessErr
}

func (m *mockGHReader) ReadIssue(_ string, issueNum int) (IssueDetails, error) {
	return IssueDetails{Number: issueNum, State: "open"}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// drain reads all available events from the channel within a timeout, skipping
// the per-cycle EventPollCycleDone sentinels (those are tested separately).
func drain(ch <-chan ChangeEvent, timeout time.Duration) []ChangeEvent {
	var out []ChangeEvent
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			if ev.Type == EventPollCycleDone {
				continue
			}
			out = append(out, ev)
		case <-timer.C:
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestPreCheck_Success(t *testing.T) {
	gh := &mockGHReader{}
	p := NewPoller(gh, testStore(t), "owner/repo", 30*time.Second)
	if err := p.PreCheck(); err != nil {
		t.Fatalf("PreCheck should succeed: %v", err)
	}
}

func TestPreCheck_Failure(t *testing.T) {
	gh := &mockGHReader{accessErr: fmt.Errorf("HTTP 404: Not Found")}
	p := NewPoller(gh, testStore(t), "owner/repo", 30*time.Second)
	err := p.PreCheck()
	if err == nil {
		t.Fatal("PreCheck should fail when no access")
	}
}

func TestNewIssueDetection(t *testing.T) {
	gh := &mockGHReader{
		issues: []Issue{
			{Number: 1, Title: "Bug report", State: "open", Labels: []string{"bug"}},
		},
	}
	st := testStore(t)
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = p.Run(ctx)
	}()

	events := drain(p.Events(), 500*time.Millisecond)
	cancel()

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Type != EventIssueCreated {
		t.Errorf("expected issue_created, got %s", events[0].Type)
	}
	if events[0].IssueNum != 1 {
		t.Errorf("expected issue 1, got %d", events[0].IssueNum)
	}
}

func TestLabelAddedRemoved(t *testing.T) {
	st := testStore(t)

	// Seed the cache with an issue that has label "bug".
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 1,
		Labels:   `["bug"]`,
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}

	// Now the issue has labels "enhancement" (bug removed, enhancement added).
	gh := &mockGHReader{
		issues: []Issue{
			{Number: 1, Title: "Bug report", State: "open", Labels: []string{"enhancement"}},
		},
	}
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = p.Run(ctx)
	}()

	events := drain(p.Events(), 500*time.Millisecond)
	cancel()

	if len(events) != 2 {
		t.Fatalf("expected 2 events (add + remove), got %d: %+v", len(events), events)
	}

	types := map[string]string{}
	for _, ev := range events {
		types[ev.Type] = ev.Detail
	}
	if types[EventLabelAdded] != "enhancement" {
		t.Errorf("expected label_added=enhancement, got %q", types[EventLabelAdded])
	}
	if types[EventLabelRemoved] != "bug" {
		t.Errorf("expected label_removed=bug, got %q", types[EventLabelRemoved])
	}
}

func TestNoChangeNoDuplicate(t *testing.T) {
	st := testStore(t)

	// Seed cache matching current state.
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 1,
		Labels:   `["bug"]`,
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}

	gh := &mockGHReader{
		issues: []Issue{
			{Number: 1, Title: "Bug report", State: "open", Labels: []string{"bug"}},
		},
	}
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = p.Run(ctx)
	}()

	events := drain(p.Events(), 500*time.Millisecond)
	cancel()

	if len(events) != 0 {
		t.Fatalf("expected 0 events for unchanged issue, got %d: %+v", len(events), events)
	}
}

func TestPRCreated(t *testing.T) {
	gh := &mockGHReader{
		prs: []PR{
			{Number: 10, URL: "https://github.com/owner/repo/pull/10", Branch: "feature", State: "open"},
		},
	}
	st := testStore(t)
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = p.Run(ctx)
	}()

	events := drain(p.Events(), 500*time.Millisecond)
	cancel()

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Type != EventPRCreated {
		t.Errorf("expected pr_created, got %s", events[0].Type)
	}
}

func TestPRStateChanged(t *testing.T) {
	st := testStore(t)

	// Seed cache with PR in open state.
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 10,
		Labels:   "",
		State:    "pr:open",
	}); err != nil {
		t.Fatal(err)
	}

	gh := &mockGHReader{
		prs: []PR{
			{Number: 10, URL: "https://github.com/owner/repo/pull/10", Branch: "feature", State: "merged"},
		},
	}
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = p.Run(ctx)
	}()

	events := drain(p.Events(), 500*time.Millisecond)
	cancel()

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Type != EventPRStateChanged {
		t.Errorf("expected pr_state_changed, got %s", events[0].Type)
	}
}

func TestRateLimitBackoff(t *testing.T) {
	gh := &mockGHReader{
		issuesErr: fmt.Errorf("HTTP 403: rate limit exceeded"),
	}
	st := testStore(t)
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	// Simulate a poll that triggers rate limit.
	p.poll(context.Background())

	if p.Backoff() != 60*time.Second {
		t.Errorf("expected 60s initial backoff, got %s", p.Backoff())
	}

	// Second rate limit doubles it.
	p.poll(context.Background())
	if p.Backoff() != 120*time.Second {
		t.Errorf("expected 120s backoff, got %s", p.Backoff())
	}

	// Keep doubling until max.
	for i := 0; i < 10; i++ {
		p.poll(context.Background())
	}
	if p.Backoff() > p.maxBackoff {
		t.Errorf("backoff %s exceeds max %s", p.Backoff(), p.maxBackoff)
	}
}

func TestGHErrorContinues(t *testing.T) {
	gh := &mockGHReader{
		issuesErr: fmt.Errorf("network timeout"),
	}
	st := testStore(t)
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	// Should not panic.
	p.poll(context.Background())

	// Backoff should NOT be set for non-rate-limit errors.
	if p.Backoff() != 0 {
		t.Errorf("expected 0 backoff for non-rate-limit error, got %s", p.Backoff())
	}
}

func TestContextCancellation(t *testing.T) {
	gh := &mockGHReader{}
	st := testStore(t)
	p := NewPoller(gh, st, "owner/repo", 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- p.Run(ctx)
	}()

	// Let it run briefly, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run should return nil on cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestDefaultInterval(t *testing.T) {
	gh := &mockGHReader{}
	p := NewPoller(gh, testStore(t), "owner/repo", 0)
	if p.interval != 30*time.Second {
		t.Errorf("expected default 30s interval, got %s", p.interval)
	}
}

func TestCrashRecovery_FullSync(t *testing.T) {
	st := testStore(t)

	// Seed cache with old data: issue 1 had label "bug".
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 1,
		Labels:   `["bug"]`,
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate restart: GitHub now shows issue 1 with "bug" + "wontfix",
	// plus a brand new issue 2.
	gh := &mockGHReader{
		issues: []Issue{
			{Number: 1, Title: "Old issue", State: "open", Labels: []string{"bug", "wontfix"}},
			{Number: 2, Title: "New issue", State: "open", Labels: []string{"enhancement"}},
		},
	}
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = p.Run(ctx)
	}()

	events := drain(p.Events(), 500*time.Millisecond)
	cancel()

	// Expect: label_added "wontfix" for issue 1, issue_created for issue 2.
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}

	types := map[string]bool{}
	for _, ev := range events {
		types[ev.Type] = true
	}
	if !types[EventLabelAdded] {
		t.Error("expected label_added event")
	}
	if !types[EventIssueCreated] {
		t.Error("expected issue_created event")
	}
}

func TestDiffLabels(t *testing.T) {
	added, removed := diffLabels(
		[]string{"a", "b", "c"},
		[]string{"b", "c", "d"},
	)
	if len(added) != 1 || added[0] != "d" {
		t.Errorf("expected added=[d], got %v", added)
	}
	if len(removed) != 1 || removed[0] != "a" {
		t.Errorf("expected removed=[a], got %v", removed)
	}
}

func TestLabelsJSON(t *testing.T) {
	json := labelsToJSON([]string{"z", "a", "m"})
	labels := labelsFromJSON(json)
	// Should be sorted.
	if len(labels) != 3 || labels[0] != "a" || labels[1] != "m" || labels[2] != "z" {
		t.Errorf("expected sorted [a m z], got %v", labels)
	}
}

func TestLabelsFromJSON_Empty(t *testing.T) {
	labels := labelsFromJSON("")
	if labels != nil {
		t.Errorf("expected nil for empty string, got %v", labels)
	}
}

func TestIssueClosedDetection(t *testing.T) {
	st := testStore(t)

	// Seed cache: issue 1 and issue 2 are known open issues.
	for _, ic := range []store.IssueCache{
		{Repo: "owner/repo", IssueNum: 1, Labels: `["bug"]`, State: "open"},
		{Repo: "owner/repo", IssueNum: 2, Labels: `["feature"]`, State: "open"},
	} {
		if err := st.UpsertIssueCache(ic); err != nil {
			t.Fatal(err)
		}
	}

	// GitHub now only returns issue 1 — issue 2 has been closed.
	gh := &mockGHReader{
		issues: []Issue{
			{Number: 1, Title: "Bug report", State: "open", Labels: []string{"bug"}},
		},
	}
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = p.Run(ctx)
	}()

	events := drain(p.Events(), 500*time.Millisecond)
	cancel()

	// Should get exactly one EventIssueClosed for issue 2.
	var closeEvents []ChangeEvent
	for _, ev := range events {
		if ev.Type == EventIssueClosed {
			closeEvents = append(closeEvents, ev)
		}
	}
	if len(closeEvents) != 1 {
		t.Fatalf("expected 1 issue_closed event, got %d: %+v", len(closeEvents), events)
	}
	if closeEvents[0].IssueNum != 2 {
		t.Errorf("expected issue_closed for issue 2, got %d", closeEvents[0].IssueNum)
	}

	// Cache entry for issue 2 should have been deleted.
	ic, err := st.QueryIssueCache("owner/repo", 2)
	if err != nil {
		t.Fatal(err)
	}
	if ic != nil {
		t.Errorf("expected cache entry for issue 2 to be deleted, but it still exists")
	}
}

func TestIssueClosedDoesNotAffectPRs(t *testing.T) {
	st := testStore(t)

	// Seed cache: issue 1 (open) and PR 10 (pr:open).
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo: "owner/repo", IssueNum: 1, Labels: `["bug"]`, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo: "owner/repo", IssueNum: 10, Labels: "", State: "pr:open",
	}); err != nil {
		t.Fatal(err)
	}

	// GitHub returns issue 1 still open, PR 10 still open.
	gh := &mockGHReader{
		issues: []Issue{
			{Number: 1, Title: "Bug report", State: "open", Labels: []string{"bug"}},
		},
		prs: []PR{
			{Number: 10, URL: "https://github.com/owner/repo/pull/10", Branch: "feature", State: "open"},
		},
	}
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = p.Run(ctx)
	}()

	events := drain(p.Events(), 500*time.Millisecond)
	cancel()

	// Should have no close events — PR should not trigger issue_closed.
	for _, ev := range events {
		if ev.Type == EventIssueClosed {
			t.Errorf("unexpected issue_closed event: %+v", ev)
		}
	}
}

// TestPollEmitsCycleDoneSentinel verifies that every successful poll cycle
// ends with an EventPollCycleDone event so consumers can use it as a per-cycle
// boundary signal (e.g. clearing dedup state in the state machine). Without
// this, a label re-added in a later cycle would be silently deduped against
// the original add, breaking review→developing retry loops.
func TestPollEmitsCycleDoneSentinel(t *testing.T) {
	st := testStore(t)
	gh := &mockGHReader{
		issues: []Issue{{Number: 1, Title: "x", State: "open", Labels: []string{"bug"}}},
	}
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	var sawCycleDone bool
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	for !sawCycleDone {
		select {
		case ev := <-p.Events():
			if ev.Type == EventPollCycleDone {
				sawCycleDone = true
			}
		case <-timer.C:
			t.Fatalf("did not observe EventPollCycleDone within timeout")
		}
	}
}

func TestPollTruncatedIssueListStillEmitsBufferedEvents(t *testing.T) {
	st := testStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 1,
		Labels:   `["bug"]`,
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}

	issues := make([]Issue, 0, ghListLimit)
	issues = append(issues, Issue{Number: 1, Title: "Bug report", State: "open", Labels: []string{"enhancement"}})
	for i := 2; i <= ghListLimit; i++ {
		issues = append(issues, Issue{Number: i, Title: fmt.Sprintf("Issue %d", i), State: "open"})
	}

	gh := &mockGHReader{
		issues: issues,
		prs: []PR{
			{Number: 10, URL: "https://github.com/owner/repo/pull/10", Branch: "feature", State: "open"},
		},
	}
	p := NewPoller(gh, st, "owner/repo", time.Hour)

	ctx := context.Background()
	p.poll(ctx)
	events := drain(p.Events(), 500*time.Millisecond)

	var sawLabelAdded, sawLabelRemoved, sawPREvent, sawUnexpectedClose bool
	for _, ev := range events {
		switch {
		case ev.Type == EventLabelAdded && ev.IssueNum == 1 && ev.Detail == "enhancement":
			sawLabelAdded = true
		case ev.Type == EventLabelRemoved && ev.IssueNum == 1 && ev.Detail == "bug":
			sawLabelRemoved = true
		case (ev.Type == EventPRCreated || ev.Type == EventPRStateChanged) && ev.IssueNum == 10:
			sawPREvent = true
		case ev.Type == EventIssueClosed:
			sawUnexpectedClose = true
		}
	}
	if !sawLabelAdded || !sawLabelRemoved || !sawPREvent {
		t.Fatalf("expected buffered label/pr events to flush under truncation, got %+v", events)
	}
	if sawUnexpectedClose {
		t.Fatalf("unexpected issue_closed under truncated issue list: %+v", events)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
