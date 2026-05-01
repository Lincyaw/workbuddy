package store

import (
	"path/filepath"
	"testing"
)

// TestSessionRouteRoundtrip exercises the upsert/get round-trip for the
// session_routes index introduced as REQ-122's follow-up. The resolver
// uses GetSessionRoute as its primary lookup; UpsertSessionRoute is
// idempotent so worker re-announce on Register is cheap.
func TestSessionRouteRoundtrip(t *testing.T) {
	s := newTestStore(t)

	route := SessionRoute{
		SessionID: "sess-1",
		WorkerID:  "worker-a",
		Repo:      "org/repo",
		IssueNum:  42,
	}
	if err := s.UpsertSessionRoute(route); err != nil {
		t.Fatalf("UpsertSessionRoute: %v", err)
	}
	got, err := s.GetSessionRoute("sess-1")
	if err != nil {
		t.Fatalf("GetSessionRoute: %v", err)
	}
	if got == nil {
		t.Fatalf("GetSessionRoute returned nil")
	}
	if got.WorkerID != "worker-a" || got.Repo != "org/repo" || got.IssueNum != 42 {
		t.Fatalf("GetSessionRoute = %+v, want worker-a/org/repo/42", got)
	}

	// Idempotency: re-upsert with a *different* worker_id is a no-op
	// (ON CONFLICT DO NOTHING). The route table is append-only by
	// design — the owning worker doesn't change for the lifetime of a
	// session_id, so a re-announce shouldn't be allowed to silently
	// rewire the route.
	if err := s.UpsertSessionRoute(SessionRoute{
		SessionID: "sess-1",
		WorkerID:  "worker-b",
		Repo:      "other/repo",
		IssueNum:  99,
	}); err != nil {
		t.Fatalf("UpsertSessionRoute (re-announce): %v", err)
	}
	got, err = s.GetSessionRoute("sess-1")
	if err != nil || got == nil {
		t.Fatalf("GetSessionRoute after re-announce: %v %+v", err, got)
	}
	if got.WorkerID != "worker-a" {
		t.Fatalf("worker_id flipped to %q after re-announce; want stable worker-a", got.WorkerID)
	}
}

// TestSessionRouteMissingReturnsNil — Resolver depends on
// (nil, nil) for "no such route" so it can fall back to the legacy
// sessions table read for serve mode (and emit ErrSessionNotRouted
// when both miss). An sql.ErrNoRows leak here would surface as 500.
func TestSessionRouteMissingReturnsNil(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetSessionRoute("nope")
	if err != nil {
		t.Fatalf("GetSessionRoute on missing row: %v", err)
	}
	if got != nil {
		t.Fatalf("GetSessionRoute on missing row = %+v, want nil", got)
	}
}

// TestBulkUpsertSessionRoutes covers the worker-Register re-seed path:
// the coordinator does one transactional bulk upsert per registering
// worker. Empty session_id entries are silently skipped (the coord
// shouldn't fail register over a malformed announce).
func TestBulkUpsertSessionRoutes(t *testing.T) {
	s := newTestStore(t)

	// Pre-existing row that must survive untouched (ON CONFLICT DO NOTHING).
	if err := s.UpsertSessionRoute(SessionRoute{SessionID: "keep", WorkerID: "worker-a", Repo: "org/r", IssueNum: 1}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := s.BulkUpsertSessionRoutes([]SessionRoute{
		{SessionID: "keep", WorkerID: "worker-different", Repo: "x/y", IssueNum: 9}, // ignored on conflict
		{SessionID: "new-1", WorkerID: "worker-a", Repo: "org/r", IssueNum: 2},
		{SessionID: "", WorkerID: "worker-a"}, // skipped
		{SessionID: "new-2", WorkerID: ""},    // skipped
	}); err != nil {
		t.Fatalf("BulkUpsertSessionRoutes: %v", err)
	}

	keep, err := s.GetSessionRoute("keep")
	if err != nil || keep == nil || keep.WorkerID != "worker-a" {
		t.Fatalf("keep route = %+v err=%v", keep, err)
	}
	got, err := s.GetSessionRoute("new-1")
	if err != nil || got == nil || got.WorkerID != "worker-a" || got.IssueNum != 2 {
		t.Fatalf("new-1 = %+v err=%v", got, err)
	}
	if missing, _ := s.GetSessionRoute("new-2"); missing != nil {
		t.Fatalf("new-2 should not have been inserted (empty worker_id), got %+v", missing)
	}
}

// TestSessionRoutesPersistOnCoordinatorStore — the coord path drops the
// legacy `sessions` table on startup but session_routes must NOT be
// dropped: the resolver depends on it. This test opens a coordinator
// store and round-trips a route to prove the table survives.
func TestSessionRoutesPersistOnCoordinatorStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "coord.db")
	s, err := NewCoordinatorStore(dbPath)
	if err != nil {
		t.Fatalf("NewCoordinatorStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.UpsertSessionRoute(SessionRoute{SessionID: "sess", WorkerID: "w", Repo: "org/r", IssueNum: 7}); err != nil {
		t.Fatalf("UpsertSessionRoute on coord store: %v", err)
	}
	got, err := s.GetSessionRoute("sess")
	if err != nil || got == nil || got.WorkerID != "w" {
		t.Fatalf("GetSessionRoute on coord store = %+v err=%v", got, err)
	}
}
