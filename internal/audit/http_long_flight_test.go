package audit

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func newAuditTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewStore(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestQueryIssueStateLongFlight: an issue first dispatched > 4h ago that is
// still in an intermediate state must surface long_flight=true even when no
// per-state stuck signal would fire.
func TestQueryIssueStateLongFlight(t *testing.T) {
	st := newAuditTestStore(t)
	repo := "owner/repo"
	issueNum := 1

	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if err := st.TouchIssueFirstDispatch(repo, issueNum); err != nil {
		t.Fatalf("TouchIssueFirstDispatch: %v", err)
	}

	// Insert a recent event so the dwell-time signal would NOT trip.
	if _, err := st.InsertEvent(store.Event{
		Type:     eventlog.TypeStateEntry,
		Repo:     repo,
		IssueNum: issueNum,
		Payload:  `{}`,
	}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	h := &HTTPHandler{
		store: st,
		// Pretend now() is 5h after first dispatch.
		now: func() time.Time { return time.Now().Add(5 * time.Hour) },
	}
	resp, err := h.queryIssueState(repo, issueNum)
	if err != nil {
		t.Fatalf("queryIssueState: %v", err)
	}
	if !resp.LongFlight {
		t.Fatalf("LongFlight = false, want true")
	}
	if !resp.Stuck {
		t.Fatalf("Stuck = false, want true (surfaced via long_flight)")
	}
	if !strings.Contains(resp.StuckReason, "long_flight") {
		t.Fatalf("StuckReason = %q, want it to contain long_flight", resp.StuckReason)
	}
}

// TestQueryIssueStateNoLongFlightWhenRecent: an issue first dispatched < 4h
// ago must not be flagged as long-flight regardless of dwell.
func TestQueryIssueStateNoLongFlightWhenRecent(t *testing.T) {
	st := newAuditTestStore(t)
	repo := "owner/repo"
	issueNum := 2

	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if err := st.TouchIssueFirstDispatch(repo, issueNum); err != nil {
		t.Fatalf("TouchIssueFirstDispatch: %v", err)
	}
	if _, err := st.InsertEvent(store.Event{
		Type:     eventlog.TypeStateEntry,
		Repo:     repo,
		IssueNum: issueNum,
		Payload:  `{}`,
	}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	h := &HTTPHandler{store: st, now: func() time.Time { return time.Now().Add(30 * time.Minute) }}
	resp, err := h.queryIssueState(repo, issueNum)
	if err != nil {
		t.Fatalf("queryIssueState: %v", err)
	}
	if resp.LongFlight {
		t.Fatalf("LongFlight = true, want false")
	}
	if resp.Stuck {
		t.Fatalf("Stuck = true, want false")
	}
}

// TestQueryIssueStateExposesDevReviewCycleCount: the audit response includes
// the dev_review_cycle_count from issue_cycle_state.
func TestQueryIssueStateExposesDevReviewCycleCount(t *testing.T) {
	st := newAuditTestStore(t)
	repo := "owner/repo"
	issueNum := 3

	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := st.IncrementDevReviewCycleCount(repo, issueNum); err != nil {
			t.Fatalf("IncrementDevReviewCycleCount: %v", err)
		}
	}
	if _, err := st.InsertEvent(store.Event{
		Type:     eventlog.TypeStateEntry,
		Repo:     repo,
		IssueNum: issueNum,
		Payload:  `{}`,
	}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	h := &HTTPHandler{store: st, now: time.Now}
	resp, err := h.queryIssueState(repo, issueNum)
	if err != nil {
		t.Fatalf("queryIssueState: %v", err)
	}
	if resp.DevReviewCycleCount != 2 {
		t.Fatalf("DevReviewCycleCount = %d, want 2", resp.DevReviewCycleCount)
	}
}
