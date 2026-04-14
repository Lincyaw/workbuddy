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

// drain reads all available events from the channel within a timeout.
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
	if events[0].Type != "issue_created" {
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
	if types["label_added"] != "enhancement" {
		t.Errorf("expected label_added=enhancement, got %q", types["label_added"])
	}
	if types["label_removed"] != "bug" {
		t.Errorf("expected label_removed=bug, got %q", types["label_removed"])
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
	if events[0].Type != "pr_created" {
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
	if events[0].Type != "pr_state_changed" {
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
	if !types["label_added"] {
		t.Error("expected label_added event")
	}
	if !types["issue_created"] {
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

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
