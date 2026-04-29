package store

import (
	"path/filepath"
	"testing"
)

func newCycleTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "cycle.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestIncrementDevReviewCycleCount(t *testing.T) {
	st := newCycleTestStore(t)
	repo := "owner/repo"
	issue := 1

	for want := 1; want <= 3; want++ {
		got, err := st.IncrementDevReviewCycleCount(repo, issue)
		if err != nil {
			t.Fatalf("IncrementDevReviewCycleCount %d: %v", want, err)
		}
		if got != want {
			t.Fatalf("count = %d, want %d", got, want)
		}
	}

	state, err := st.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("QueryIssueCycleState: %v", err)
	}
	if state == nil || state.DevReviewCycleCount != 3 {
		t.Fatalf("state = %+v", state)
	}
}

func TestQueryIssueCycleStateMissing(t *testing.T) {
	st := newCycleTestStore(t)
	state, err := st.QueryIssueCycleState("owner/repo", 99)
	if err != nil {
		t.Fatalf("QueryIssueCycleState: %v", err)
	}
	if state != nil {
		t.Fatalf("expected nil state for missing issue, got %+v", state)
	}
}

func TestTouchIssueFirstDispatchIsIdempotent(t *testing.T) {
	st := newCycleTestStore(t)
	repo := "owner/repo"
	issue := 2

	if err := st.TouchIssueFirstDispatch(repo, issue); err != nil {
		t.Fatalf("first touch: %v", err)
	}
	first, err := st.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if first == nil || first.FirstDispatchAt.IsZero() {
		t.Fatalf("first_dispatch_at not set: %+v", first)
	}
	originalFirst := first.FirstDispatchAt

	if err := st.TouchIssueFirstDispatch(repo, issue); err != nil {
		t.Fatalf("second touch: %v", err)
	}
	second, err := st.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !second.FirstDispatchAt.Equal(originalFirst) {
		t.Fatalf("first_dispatch_at changed: original=%v second=%v", originalFirst, second.FirstDispatchAt)
	}
}

func TestMarkIssueCycleCapHitIsIdempotent(t *testing.T) {
	st := newCycleTestStore(t)
	repo := "owner/repo"
	issue := 3

	if err := st.MarkIssueCycleCapHit(repo, issue); err != nil {
		t.Fatalf("MarkIssueCycleCapHit: %v", err)
	}
	first, err := st.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if first == nil || first.CapHitAt.IsZero() {
		t.Fatalf("cap_hit_at not set: %+v", first)
	}
	originalHit := first.CapHitAt

	if err := st.MarkIssueCycleCapHit(repo, issue); err != nil {
		t.Fatalf("second MarkIssueCycleCapHit: %v", err)
	}
	second, err := st.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !second.CapHitAt.Equal(originalHit) {
		t.Fatalf("cap_hit_at changed: original=%v second=%v", originalHit, second.CapHitAt)
	}
}

func TestResetIssueCycleStateRemovesRow(t *testing.T) {
	st := newCycleTestStore(t)
	repo := "owner/repo"
	issue := 4

	if _, err := st.IncrementDevReviewCycleCount(repo, issue); err != nil {
		t.Fatalf("IncrementDevReviewCycleCount: %v", err)
	}
	if err := st.ResetIssueCycleState(repo, issue); err != nil {
		t.Fatalf("ResetIssueCycleState: %v", err)
	}
	state, err := st.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != nil {
		t.Fatalf("expected nil after reset, got %+v", state)
	}
}
