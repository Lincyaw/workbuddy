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
