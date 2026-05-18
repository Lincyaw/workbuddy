//go:build faultinject

package store

import (
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/failpoints"
)

// TestInsertEventBusyOnceRetriesOnce arms the `store.insert_event.busy`
// failpoint with `once` so the first iteration of the retry loop fails with
// a synthetic SQLITE_BUSY error and the second iteration succeeds against
// the real DB. This is the regression test for the major-1 fix in #345 /
// REQ-153: pre-fix the Hit lived above the retry loop and returned
// immediately, so the loop was never reached.
func TestInsertEventBusyOnceRetriesOnce(t *testing.T) {
	if !failpoints.Enabled() {
		t.Skip("requires -tags faultinject")
	}
	failpoints.Reset()
	t.Cleanup(failpoints.Reset)

	s := newTestStore(t)

	// Arm a one-shot busy error. IsBusyError matches "sqlite_busy"
	// case-insensitively so the retry path engages.
	failpoints.Arm("store.insert_event.busy", failpoints.Effect{
		Kind: "error",
		Err:  "SQLITE_BUSY (injected)",
		Once: true,
	})

	id, err := s.InsertEvent(Event{
		Type:     "test",
		Repo:     "owner/repo",
		IssueNum: 1,
		Payload:  `{}`,
	})
	if err != nil {
		t.Fatalf("InsertEvent: expected retry-then-success, got %v", err)
	}
	if id <= 0 {
		t.Fatalf("InsertEvent returned non-positive id %d", id)
	}

	// The row must exist in the DB — the second iteration ran the real Exec.
	evs, err := s.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 event after retry-success, got %d", len(evs))
	}
}

// TestInsertEventBusyPermanentExhaustsRetries arms the failpoint without
// `once`, so every iteration of the retry loop hits the injected SQLITE_BUSY
// error. The call must surface a wrapped error after exhausting the
// four-attempt schedule. This pins that the failpoint observes ALL iterations
// of the loop (the pre-fix behaviour was a single attempt that short-circuited
// before the loop).
func TestInsertEventBusyPermanentExhaustsRetries(t *testing.T) {
	if !failpoints.Enabled() {
		t.Skip("requires -tags faultinject")
	}
	failpoints.Reset()
	t.Cleanup(failpoints.Reset)

	s := newTestStore(t)

	failpoints.Arm("store.insert_event.busy", failpoints.Effect{
		Kind: "error",
		Err:  "SQLITE_BUSY (permanent)",
	})

	_, err := s.InsertEvent(Event{
		Type:     "test",
		Repo:     "owner/repo",
		IssueNum: 1,
		Payload:  `{}`,
	})
	if err == nil {
		t.Fatalf("InsertEvent: expected error after exhausted retries, got nil")
	}
	// The wrapped final error must carry the injected message — proves
	// the loop walked all iterations and lastErr held the most recent
	// failpoint emission rather than something else.
	if !strings.Contains(err.Error(), "SQLITE_BUSY (permanent)") {
		t.Fatalf("InsertEvent error %q does not contain injected message", err)
	}

	// No row should have been written.
	evs, err := s.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("expected 0 events after permanent failpoint, got %d", len(evs))
	}
}

// TestInsertEventBusyNonBusyErrorBreaksImmediately arms the failpoint with a
// non-busy error message so IsBusyError returns false. The loop must break
// after the first iteration rather than retry — same semantics as a real
// non-transient driver error.
func TestInsertEventBusyNonBusyErrorBreaksImmediately(t *testing.T) {
	if !failpoints.Enabled() {
		t.Skip("requires -tags faultinject")
	}
	failpoints.Reset()
	t.Cleanup(failpoints.Reset)

	s := newTestStore(t)

	failpoints.Arm("store.insert_event.busy", failpoints.Effect{
		Kind: "error",
		Err:  "not a busy error: synthetic",
	})

	_, err := s.InsertEvent(Event{
		Type:     "test",
		Repo:     "owner/repo",
		IssueNum: 1,
		Payload:  `{}`,
	})
	if err == nil {
		t.Fatalf("expected non-busy error to surface immediately, got nil")
	}
	if !strings.Contains(err.Error(), "not a busy error: synthetic") {
		t.Fatalf("unexpected error: %v", err)
	}
}
