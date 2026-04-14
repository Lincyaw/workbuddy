package store

import (
	"path/filepath"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sub", "test.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestCreateAndReadWrite verifies basic CRUD for all tables.
func TestCreateAndReadWrite(t *testing.T) {
	s := newTestStore(t)

	// --- Events ---
	id, err := s.InsertEvent(Event{Type: "poll", Repo: "org/repo", IssueNum: 1, Payload: `{"a":1}`})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if id != 1 {
		t.Fatalf("expected id 1, got %d", id)
	}
	events, err := s.QueryEvents("org/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 || events[0].Type != "poll" {
		t.Fatalf("unexpected events: %+v", events)
	}
	// Query all repos.
	allEvents, err := s.QueryEvents("")
	if err != nil {
		t.Fatalf("QueryEvents all: %v", err)
	}
	if len(allEvents) != 1 {
		t.Fatalf("expected 1 event, got %d", len(allEvents))
	}

	// --- Tasks ---
	err = s.InsertTask(TaskRecord{ID: "task-1", Repo: "org/repo", IssueNum: 1, AgentName: "dev", Status: TaskStatusPending})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	tasks, err := s.QueryTasks(TaskStatusPending)
	if err != nil {
		t.Fatalf("QueryTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].AgentName != "dev" {
		t.Fatalf("unexpected tasks: %+v", tasks)
	}
	// Update status.
	if err := s.UpdateTaskStatus("task-1", TaskStatusRunning); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}
	tasks, _ = s.QueryTasks(TaskStatusPending)
	if len(tasks) != 0 {
		t.Fatalf("expected 0 pending tasks after update, got %d", len(tasks))
	}
	tasks, _ = s.QueryTasks(TaskStatusRunning)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 running task, got %d", len(tasks))
	}

	// --- Workers ---
	err = s.InsertWorker(WorkerRecord{ID: "w-1", Repo: "org/repo", Roles: `["dev"]`, Hostname: "host1", Status: "online"})
	if err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	workers, err := s.QueryWorkers("org/repo")
	if err != nil {
		t.Fatalf("QueryWorkers: %v", err)
	}
	if len(workers) != 1 || workers[0].Hostname != "host1" {
		t.Fatalf("unexpected workers: %+v", workers)
	}
	if err := s.UpdateWorkerHeartbeat("w-1"); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat: %v", err)
	}
	if err := s.UpdateWorkerStatus("w-1", "offline"); err != nil {
		t.Fatalf("UpdateWorkerStatus: %v", err)
	}
	workers, _ = s.QueryWorkers("")
	if workers[0].Status != "offline" {
		t.Fatalf("expected offline, got %s", workers[0].Status)
	}

	// --- Transition Counts ---
	cnt, err := s.IncrementTransition("org/repo", 1, "reviewing", "developing")
	if err != nil {
		t.Fatalf("IncrementTransition: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("expected count 1, got %d", cnt)
	}
	cnt, err = s.IncrementTransition("org/repo", 1, "reviewing", "developing")
	if err != nil {
		t.Fatalf("IncrementTransition 2: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("expected count 2, got %d", cnt)
	}
	tcs, err := s.QueryTransitionCounts("org/repo", 1)
	if err != nil {
		t.Fatalf("QueryTransitionCounts: %v", err)
	}
	if len(tcs) != 1 || tcs[0].Count != 2 {
		t.Fatalf("unexpected transition counts: %+v", tcs)
	}

	// --- Issue Cache ---
	err = s.UpsertIssueCache(IssueCache{Repo: "org/repo", IssueNum: 1, Labels: `["bug"]`, State: "open"})
	if err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	ic, err := s.QueryIssueCache("org/repo", 1)
	if err != nil {
		t.Fatalf("QueryIssueCache: %v", err)
	}
	if ic == nil || ic.State != "open" {
		t.Fatalf("unexpected issue cache: %+v", ic)
	}
	// Upsert update.
	err = s.UpsertIssueCache(IssueCache{Repo: "org/repo", IssueNum: 1, Labels: `["bug","wip"]`, State: "open"})
	if err != nil {
		t.Fatalf("UpsertIssueCache update: %v", err)
	}
	ic, _ = s.QueryIssueCache("org/repo", 1)
	if ic.Labels != `["bug","wip"]` {
		t.Fatalf("expected updated labels, got %s", ic.Labels)
	}
	// Cache miss.
	ic, err = s.QueryIssueCache("org/repo", 999)
	if err != nil {
		t.Fatalf("QueryIssueCache miss: %v", err)
	}
	if ic != nil {
		t.Fatalf("expected nil for cache miss, got %+v", ic)
	}

	// --- Agent Sessions ---
	sessID, err := s.InsertAgentSession(AgentSession{
		SessionID: "sess-1", TaskID: "task-1", Repo: "org/repo",
		IssueNum: 1, AgentName: "dev", Summary: "ok", RawPath: "/tmp/raw",
	})
	if err != nil {
		t.Fatalf("InsertAgentSession: %v", err)
	}
	if sessID != 1 {
		t.Fatalf("expected session id 1, got %d", sessID)
	}
	sessions, err := s.QueryAgentSessions("org/repo", 1)
	if err != nil {
		t.Fatalf("QueryAgentSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Summary != "ok" {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
}

// TestIncrementTransitionAtomic verifies that concurrent IncrementTransition
// calls produce the correct final count (no lost updates).
func TestIncrementTransitionAtomic(t *testing.T) {
	s := newTestStore(t)

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	counts := make(chan int, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cnt, err := s.IncrementTransition("org/repo", 1, "dev", "review")
			if err != nil {
				errs <- err
				return
			}
			counts <- cnt
		}()
	}
	wg.Wait()
	close(errs)
	close(counts)

	for err := range errs {
		t.Fatalf("concurrent IncrementTransition failed: %v", err)
	}

	// Final count must be exactly n.
	tcs, err := s.QueryTransitionCounts("org/repo", 1)
	if err != nil {
		t.Fatalf("QueryTransitionCounts: %v", err)
	}
	if len(tcs) != 1 || tcs[0].Count != n {
		t.Fatalf("expected final count %d, got %+v", n, tcs)
	}

	// Every returned count should be unique (1..n) if truly atomic.
	seen := make(map[int]bool)
	for c := range counts {
		if c < 1 || c > n {
			t.Errorf("count %d out of range [1, %d]", c, n)
		}
		seen[c] = true
	}
	if len(seen) != n {
		t.Errorf("expected %d unique counts, got %d (some increments were not atomic)", n, len(seen))
	}
}

// TestParseTimestamp verifies the multi-format timestamp parser.
func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"2026-04-14 13:00:05", true},
		{"2026-04-14T13:00:05Z", true},
		{"2026-04-14T13:00:05+08:00", true},
		{"2026-04-14T13:00:05", true},
		{"not-a-date", false},
		{"", false},
	}
	for _, tt := range tests {
		parsed, ok := parseTimestamp(tt.input, "test")
		if ok != tt.ok {
			t.Errorf("parseTimestamp(%q): ok=%v, want %v", tt.input, ok, tt.ok)
		}
		if ok && parsed.IsZero() {
			t.Errorf("parseTimestamp(%q): returned zero time but ok=true", tt.input)
		}
	}
}

// TestConcurrentWrites verifies that concurrent inserts do not fail or corrupt data.
func TestConcurrentWrites(t *testing.T) {
	s := newTestStore(t)

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.InsertEvent(Event{Type: "concurrent", Repo: "org/repo", IssueNum: 1})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent insert failed: %v", err)
	}

	events, err := s.QueryEvents("org/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != n {
		t.Fatalf("expected %d events, got %d", n, len(events))
	}
}

// TestRestartRetention verifies data survives closing and reopening the store.
func TestRestartRetention(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "retention.db")

	// Phase 1: write data.
	s1, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore phase 1: %v", err)
	}
	_, err = s1.InsertEvent(Event{Type: "start", Repo: "org/repo", IssueNum: 1})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	err = s1.InsertTask(TaskRecord{ID: "t-1", Repo: "org/repo", IssueNum: 1, AgentName: "dev", Status: TaskStatusPending})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	_, err = s1.IncrementTransition("org/repo", 1, "a", "b")
	if err != nil {
		t.Fatalf("IncrementTransition: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("s1.Close: %v", err)
	}

	// Phase 2: reopen and verify.
	s2, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore phase 2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	events, _ := s2.QueryEvents("org/repo")
	if len(events) != 1 || events[0].Type != "start" {
		t.Fatalf("events not retained: %+v", events)
	}
	tasks, _ := s2.QueryTasks("")
	if len(tasks) != 1 || tasks[0].ID != "t-1" {
		t.Fatalf("tasks not retained: %+v", tasks)
	}
	tcs, _ := s2.QueryTransitionCounts("org/repo", 1)
	if len(tcs) != 1 || tcs[0].Count != 1 {
		t.Fatalf("transition counts not retained: %+v", tcs)
	}
}

// TestListCachedIssueNums verifies filtering of issues vs PRs.
func TestListCachedIssueNums(t *testing.T) {
	s := newTestStore(t)

	// Insert issues and PRs into cache.
	for _, ic := range []IssueCache{
		{Repo: "org/repo", IssueNum: 1, Labels: `["bug"]`, State: "open"},
		{Repo: "org/repo", IssueNum: 2, Labels: `["feature"]`, State: "open"},
		{Repo: "org/repo", IssueNum: 10, Labels: "", State: "pr:open"},   // PR — should be excluded
		{Repo: "org/repo", IssueNum: 11, Labels: "", State: "pr:merged"}, // PR — should be excluded
		{Repo: "other/repo", IssueNum: 3, Labels: `[]`, State: "open"},   // different repo
	} {
		if err := s.UpsertIssueCache(ic); err != nil {
			t.Fatalf("UpsertIssueCache: %v", err)
		}
	}

	nums, err := s.ListCachedIssueNums("org/repo")
	if err != nil {
		t.Fatalf("ListCachedIssueNums: %v", err)
	}
	if len(nums) != 2 {
		t.Fatalf("expected 2 issue nums, got %d: %v", len(nums), nums)
	}
	numSet := map[int]bool{}
	for _, n := range nums {
		numSet[n] = true
	}
	if !numSet[1] || !numSet[2] {
		t.Errorf("expected issues 1 and 2, got %v", nums)
	}
}

// TestDeleteIssueCache verifies cache deletion.
func TestDeleteIssueCache(t *testing.T) {
	s := newTestStore(t)

	if err := s.UpsertIssueCache(IssueCache{Repo: "org/repo", IssueNum: 5, Labels: `["bug"]`, State: "open"}); err != nil {
		t.Fatal(err)
	}
	// Verify it exists.
	ic, _ := s.QueryIssueCache("org/repo", 5)
	if ic == nil {
		t.Fatal("expected cached issue to exist")
	}

	// Delete it.
	if err := s.DeleteIssueCache("org/repo", 5); err != nil {
		t.Fatalf("DeleteIssueCache: %v", err)
	}

	// Verify it's gone.
	ic, err := s.QueryIssueCache("org/repo", 5)
	if err != nil {
		t.Fatalf("QueryIssueCache after delete: %v", err)
	}
	if ic != nil {
		t.Fatalf("expected nil after delete, got %+v", ic)
	}

	// Deleting a non-existent entry should not error.
	if err := s.DeleteIssueCache("org/repo", 999); err != nil {
		t.Fatalf("DeleteIssueCache non-existent: %v", err)
	}
}

// TestWALMode verifies that WAL mode is enabled.
func TestWALMode(t *testing.T) {
	s := newTestStore(t)

	var mode string
	err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode)
	if err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected WAL mode, got %q", mode)
	}
}
