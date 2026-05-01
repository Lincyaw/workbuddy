package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start.UTC()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
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
	released, err := s.ReleaseTask("task-1", "")
	if err != nil {
		t.Fatalf("ReleaseTask: %v", err)
	}
	if released {
		t.Fatal("release should require matching worker id")
	}
	claimed, err := s.ClaimTask("task-1", "worker-1")
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim to succeed")
	}
	tasks, _ = s.QueryTasks(TaskStatusPending)
	if len(tasks) != 0 {
		t.Fatalf("expected 0 pending tasks after claim, got %d", len(tasks))
	}
	tasks, _ = s.QueryTasks(TaskStatusRunning)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 running task, got %d", len(tasks))
	}
	released, err = s.ReleaseTask("task-1", "worker-1")
	if err != nil {
		t.Fatalf("ReleaseTask after claim: %v", err)
	}
	if !released {
		t.Fatal("expected release to succeed")
	}
	requeued, err := s.GetTask("task-1")
	if err != nil {
		t.Fatalf("GetTask after release: %v", err)
	}
	if requeued == nil || requeued.Status != TaskStatusPending {
		t.Fatalf("expected pending task after release, got %+v", requeued)
	}

	// --- Workers ---
	err = s.InsertWorker(WorkerRecord{ID: "w-1", Repo: "org/repo", Roles: `["dev"]`, Runtime: "codex", Hostname: "host1", Status: "online"})
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
	if workers[0].Runtime != "codex" {
		t.Fatalf("expected runtime codex, got %+v", workers[0])
	}
	if err := s.UpdateWorkerHeartbeat("w-1"); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat: %v", err)
	}
	if err := s.UpdateWorkerHeartbeat("missing-worker"); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("UpdateWorkerHeartbeat missing worker = %v, want ErrWorkerNotFound", err)
	}
	if err := s.UpdateWorkerStatus("w-1", "offline"); err != nil {
		t.Fatalf("UpdateWorkerStatus: %v", err)
	}
	if err := s.UpdateWorkerStatus("missing-worker", "offline"); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("UpdateWorkerStatus missing worker = %v, want ErrWorkerNotFound", err)
	}
	workers, _ = s.QueryWorkers("")
	if workers[0].Status != "offline" {
		t.Fatalf("expected offline, got %s", workers[0].Status)
	}
	if err := s.UpsertRepoRegistration(RepoRegistrationRecord{
		Repo:        "org/repo",
		Environment: "test",
		Status:      "active",
		ConfigJSON:  `{"repo":"org/repo"}`,
	}); err != nil {
		t.Fatalf("UpsertRepoRegistration: %v", err)
	}
	regs, err := s.ListRepoRegistrations()
	if err != nil {
		t.Fatalf("ListRepoRegistrations: %v", err)
	}
	if len(regs) != 1 || regs[0].Repo != "org/repo" {
		t.Fatalf("unexpected repo registrations: %+v", regs)
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
	err = s.UpsertIssueCache(IssueCache{Repo: "org/repo", IssueNum: 1, Labels: `["bug","wip"]`, Body: "body", State: "open"})
	if err != nil {
		t.Fatalf("UpsertIssueCache update: %v", err)
	}
	ic, _ = s.QueryIssueCache("org/repo", 1)
	if ic.Labels != `["bug","wip"]` {
		t.Fatalf("expected updated labels, got %s", ic.Labels)
	}
	if ic.Body != "body" {
		t.Fatalf("expected updated body, got %q", ic.Body)
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

	// UpdateAgentSession
	if err := s.UpdateAgentSession("sess-1", "updated summary", "/tmp/updated"); err != nil {
		t.Fatalf("UpdateAgentSession: %v", err)
	}
	sess, err := s.GetAgentSession("sess-1")
	if err != nil {
		t.Fatalf("GetAgentSession after update: %v", err)
	}
	if sess == nil || sess.Summary != "updated summary" || sess.RawPath != "/tmp/updated" {
		t.Fatalf("unexpected session after update: %+v", sess)
	}

	// --- Dependency tables ---
	err = s.ReplaceIssueDependencies("org/repo", 1, []IssueDependency{{
		Repo:              "org/repo",
		IssueNum:          1,
		DependsOnRepo:     "org/repo",
		DependsOnIssueNum: 2,
		SourceHash:        "hash-1",
		Status:            DependencyStatusActive,
	}})
	if err != nil {
		t.Fatalf("ReplaceIssueDependencies: %v", err)
	}
	deps, err := s.ListIssueDependencies("org/repo", 1)
	if err != nil {
		t.Fatalf("ListIssueDependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnIssueNum != 2 {
		t.Fatalf("unexpected dependencies: %+v", deps)
	}
	err = s.UpsertIssueDependencyState(IssueDependencyState{
		Repo:              "org/repo",
		IssueNum:          1,
		Verdict:           DependencyVerdictBlocked,
		ResumeLabel:       "status:developing",
		BlockedReasonHash: "reason",
		GraphVersion:      1,
	})
	if err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}
	depState, err := s.QueryIssueDependencyState("org/repo", 1)
	if err != nil {
		t.Fatalf("QueryIssueDependencyState: %v", err)
	}
	if depState == nil || depState.Verdict != DependencyVerdictBlocked {
		t.Fatalf("unexpected dependency state: %+v", depState)
	}
	if depState.LastReactionBlocked {
		t.Fatalf("LastReactionBlocked should default to false, got: %+v", depState)
	}
	if err := s.MarkDependencyReactionApplied("org/repo", 1, true); err != nil {
		t.Fatalf("MarkDependencyReactionApplied: %v", err)
	}
	depState, err = s.QueryIssueDependencyState("org/repo", 1)
	if err != nil {
		t.Fatalf("QueryIssueDependencyState after MarkDependencyReactionApplied: %v", err)
	}
	if !depState.LastReactionBlocked {
		t.Fatalf("LastReactionBlocked should be true after Mark, got: %+v", depState)
	}
}

func TestInsertTask_DefaultRolloutColumnsPreserveSingleTaskSemantics(t *testing.T) {
	s := newTestStore(t)

	if err := s.InsertTask(TaskRecord{
		ID:        "task-single",
		Repo:      "org/repo",
		IssueNum:  41,
		AgentName: "dev-agent",
		Status:    TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	task, err := s.GetTask("task-single")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.RolloutIndex != 0 {
		t.Fatalf("rollout_index = %d, want 0", task.RolloutIndex)
	}
	if task.RolloutsTotal != 1 {
		t.Fatalf("rollouts_total = %d, want 1", task.RolloutsTotal)
	}
	if task.RolloutGroupID != "" {
		t.Fatalf("rollout_group_id = %q, want empty", task.RolloutGroupID)
	}
}

func TestLatestRolloutGroupSummaryForIssueState_ReturnsNewestMatchingGroup(t *testing.T) {
	s := newTestStore(t)

	insert := func(id, groupID string, rolloutIndex int, status string) {
		t.Helper()
		if err := s.InsertTask(TaskRecord{
			ID:             id,
			Repo:           "org/repo",
			IssueNum:       55,
			AgentName:      "dev-agent",
			Workflow:       "default",
			State:          "developing",
			RolloutIndex:   rolloutIndex,
			RolloutsTotal:  3,
			RolloutGroupID: groupID,
			Status:         status,
		}); err != nil {
			t.Fatalf("InsertTask(%s): %v", id, err)
		}
	}

	insert("older-1", "group-older", 1, TaskStatusCompleted)
	insert("older-2", "group-older", 2, TaskStatusFailed)
	insert("newer-1", "group-newer", 1, TaskStatusCompleted)
	insert("newer-2", "group-newer", 2, TaskStatusCompleted)

	summary, err := s.LatestRolloutGroupSummaryForIssueState("org/repo", 55, "default", "developing")
	if err != nil {
		t.Fatalf("LatestRolloutGroupSummaryForIssueState: %v", err)
	}
	if summary == nil {
		t.Fatal("expected rollout group summary")
	}
	if summary.RolloutGroupID != "group-newer" {
		t.Fatalf("group_id = %q, want group-newer", summary.RolloutGroupID)
	}
	if summary.SuccessCount != 2 {
		t.Fatalf("success_count = %d, want 2", summary.SuccessCount)
	}
}

func TestListTasksForIssue_IncludesCompletedHistory(t *testing.T) {
	s := newTestStore(t)

	insert := func(id string, issueNum int, status string) {
		t.Helper()
		if err := s.InsertTask(TaskRecord{
			ID:        id,
			Repo:      "org/repo",
			IssueNum:  issueNum,
			AgentName: "dev-agent",
			Status:    status,
		}); err != nil {
			t.Fatalf("InsertTask(%s): %v", id, err)
		}
	}

	insert("task-a", 90, TaskStatusCompleted)
	insert("task-b", 90, TaskStatusFailed)
	insert("task-c", 91, TaskStatusPending)

	tasks, err := s.ListTasksForIssue("org/repo", 90)
	if err != nil {
		t.Fatalf("ListTasksForIssue: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if tasks[0].ID != "task-a" || tasks[1].ID != "task-b" {
		t.Fatalf("tasks = %+v", tasks)
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

func TestTaskClaimLifecycle(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertTask(TaskRecord{
		ID:        "task-claim",
		Repo:      "org/repo",
		IssueNum:  7,
		AgentName: "dev-agent",
		Role:      "dev",
		Runtime:   "codex",
		Workflow:  "default",
		State:     "developing",
		Status:    TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	task, err := s.ClaimNextTask("worker-a", []string{"dev"}, []string{"org/repo"}, "codex", "claim-1", 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextTask: %v", err)
	}
	if task == nil || task.ID != "task-claim" {
		t.Fatalf("unexpected claimed task: %+v", task)
	}

	sameTask, err := s.ClaimNextTask("worker-a", []string{"dev"}, []string{"org/repo"}, "codex", "claim-1", 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextTask idempotent: %v", err)
	}
	if sameTask == nil || sameTask.ID != task.ID {
		t.Fatalf("idempotent claim returned %+v, want %q", sameTask, task.ID)
	}

	none, err := s.ClaimNextTask("worker-b", []string{"dev"}, []string{"other/repo"}, "codex", "", 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextTask other worker: %v", err)
	}
	if none != nil {
		t.Fatalf("expected no task for second worker, got %+v", none)
	}

	if err := s.AckTask(task.ID, "worker-a", 30*time.Second); err != nil {
		t.Fatalf("AckTask: %v", err)
	}
	if err := s.HeartbeatTask(task.ID, "worker-a", 30*time.Second); err != nil {
		t.Fatalf("HeartbeatTask: %v", err)
	}
	if err := s.CompleteTask(task.ID, "worker-a", 0, `["session-1"]`); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got == nil || got.Status != TaskStatusCompleted || got.ExitCode != 0 || got.SessionRefs != `["session-1"]` {
		t.Fatalf("unexpected completed task: %+v", got)
	}
}

func TestTaskOwnershipConflicts(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertTask(TaskRecord{
		ID:        "task-conflict",
		Repo:      "org/repo",
		IssueNum:  8,
		AgentName: "review-agent",
		Role:      "review",
		Runtime:   "codex",
		Status:    TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	task, err := s.ClaimNextTask("worker-a", []string{"review"}, []string{"org/repo"}, "codex", "claim-review", 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected claimed task")
	}

	if err := s.AckTask(task.ID, "worker-b", 30*time.Second); !errors.Is(err, ErrTaskNotClaimedByWorker) {
		t.Fatalf("AckTask wrong worker err = %v, want %v", err, ErrTaskNotClaimedByWorker)
	}
}

func TestClaimNextTaskDoesNotSelfReclaimExpiredRunningTask(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertTask(TaskRecord{
		ID:        "task-self-reclaim",
		Repo:      "org/repo",
		IssueNum:  9,
		AgentName: "dev-agent",
		Role:      "dev",
		Runtime:   "codex",
		Status:    TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	task, err := s.ClaimNextTask("worker-a", []string{"dev"}, []string{"org/repo"}, "codex", "", 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextTask: %v", err)
	}
	if task == nil || task.ID != "task-self-reclaim" {
		t.Fatalf("unexpected claimed task: %+v", task)
	}

	if _, err := s.DB().Exec(`UPDATE task_queue SET lease_expires_at = datetime('now', '-1 second') WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	again, err := s.ClaimNextTask("worker-a", []string{"dev"}, []string{"org/repo"}, "codex", "", 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextTask self reclaim: %v", err)
	}
	if again != nil {
		t.Fatalf("expected expired running task to be hidden from original worker, got %+v", again)
	}

	other, err := s.ClaimNextTask("worker-b", []string{"dev"}, []string{"org/repo"}, "codex", "", 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextTask other worker: %v", err)
	}
	if other == nil || other.ID != task.ID {
		t.Fatalf("expected another worker to reclaim expired task, got %+v", other)
	}
}

func TestClaimNextTaskFiltersByRuntime(t *testing.T) {
	insertTask := func(t *testing.T, s *Store, id, runtime string) {
		t.Helper()
		if err := s.InsertTask(TaskRecord{
			ID:        id,
			Repo:      "org/repo",
			IssueNum:  10,
			AgentName: "dev-agent",
			Role:      "dev",
			Runtime:   runtime,
			Status:    TaskStatusPending,
		}); err != nil {
			t.Fatalf("InsertTask(%s): %v", id, err)
		}
	}

	t.Run("codex worker claims codex task", func(t *testing.T) {
		s := newTestStore(t)
		insertTask(t, s, "task-codex", "codex")
		task, err := s.ClaimNextTask("worker-codex", []string{"dev"}, []string{"org/repo"}, "codex", "", 30*time.Second)
		if err != nil {
			t.Fatalf("ClaimNextTask: %v", err)
		}
		if task == nil || task.ID != "task-codex" {
			t.Fatalf("expected codex task, got %+v", task)
		}
	})

	t.Run("codex worker does not claim claude task", func(t *testing.T) {
		s := newTestStore(t)
		insertTask(t, s, "task-claude", "claude-code")
		task, err := s.ClaimNextTask("worker-codex-2", []string{"dev"}, []string{"org/repo"}, "codex", "", 30*time.Second)
		if err != nil {
			t.Fatalf("ClaimNextTask: %v", err)
		}
		if task != nil {
			t.Fatalf("expected no task, got %+v", task)
		}
	})

	t.Run("claude worker does not claim codex task", func(t *testing.T) {
		s := newTestStore(t)
		insertTask(t, s, "task-codex-2", "codex")
		task, err := s.ClaimNextTask("worker-claude", []string{"dev"}, []string{"org/repo"}, "claude-code", "", 30*time.Second)
		if err != nil {
			t.Fatalf("ClaimNextTask: %v", err)
		}
		if task != nil {
			t.Fatalf("expected no task, got %+v", task)
		}
	})

	t.Run("empty-runtime worker only claims any-runtime task", func(t *testing.T) {
		s := newTestStore(t)
		insertTask(t, s, "task-any", "")
		task, err := s.ClaimNextTask("worker-any", []string{"dev"}, []string{"org/repo"}, "", "", 30*time.Second)
		if err != nil {
			t.Fatalf("ClaimNextTask: %v", err)
		}
		if task == nil || task.ID != "task-any" {
			t.Fatalf("expected any-runtime task, got %+v", task)
		}
	})
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
		parsed, ok := ParseTimestamp(tt.input, "test")
		if ok != tt.ok {
			t.Errorf("ParseTimestamp(%q): ok=%v, want %v", tt.input, ok, tt.ok)
		}
		if ok && parsed.IsZero() {
			t.Errorf("ParseTimestamp(%q): returned zero time but ok=true", tt.input)
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

func TestLegacyAgentSessionsMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	s1, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore phase 1: %v", err)
	}
	if _, err := s1.DB().Exec(`INSERT INTO agent_sessions (session_id, task_id, repo, issue_num, agent_name, summary, raw_path) VALUES ('legacy-1', 'task-1', 'org/repo', 7, 'dev', 'summary', '/tmp/raw')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close phase 1: %v", err)
	}

	s2, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore phase 2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	sess, err := s2.GetSession("legacy-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected migrated session")
	}
	if sess.Repo != "org/repo" || sess.IssueNum != 7 || sess.AgentName != "dev" {
		t.Fatalf("unexpected migrated session: %+v", sess)
	}
	if sess.Summary != "summary" || sess.RawPath != "/tmp/raw" {
		t.Fatalf("expected migrated summary/raw path, got %+v", sess)
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

func TestQueryTasksFilteredAndDeleteIssueDependencyState(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertIssueCache(IssueCache{Repo: "owner/repo", IssueNum: 2, Labels: `["status:reviewing","priority:high"]`, State: "open"}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	for _, task := range []TaskRecord{
		{ID: "task-1", Repo: "owner/repo", IssueNum: 1, AgentName: "dev", Status: TaskStatusPending},
		{ID: "task-2", Repo: "owner/repo", IssueNum: 2, AgentName: "dev", Status: TaskStatusFailed},
		{ID: "task-3", Repo: "other/repo", IssueNum: 3, AgentName: "dev", Status: TaskStatusFailed},
	} {
		if err := s.InsertTask(task); err != nil {
			t.Fatalf("InsertTask(%s): %v", task.ID, err)
		}
	}

	rows, err := s.QueryTasksFiltered(TaskFilter{Repo: "owner/repo", Status: TaskStatusFailed})
	if err != nil {
		t.Fatalf("QueryTasksFiltered: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "task-2" {
		t.Fatalf("unexpected filtered rows: %+v", rows)
	}
	if rows[0].Labels != `["status:reviewing","priority:high"]` {
		t.Fatalf("expected labels join, got %+v", rows[0])
	}

	if err := s.UpsertIssueDependencyState(IssueDependencyState{Repo: "owner/repo", IssueNum: 9, Verdict: DependencyVerdictReady}); err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}
	deleted, err := s.DeleteIssueDependencyState("owner/repo", 9)
	if err != nil {
		t.Fatalf("DeleteIssueDependencyState: %v", err)
	}
	if !deleted {
		t.Fatal("expected dependency state to be deleted")
	}
	deleted, err = s.DeleteIssueDependencyState("owner/repo", 999)
	if err != nil {
		t.Fatalf("DeleteIssueDependencyState miss: %v", err)
	}
	if deleted {
		t.Fatal("expected miss to report deleted=false")
	}
}

func TestCountConsecutiveAgentFailures(t *testing.T) {
	s := newTestStore(t)

	insert := func(id, agent, status string) {
		t.Helper()
		if err := s.InsertTask(TaskRecord{
			ID:        id,
			Repo:      "owner/repo",
			IssueNum:  1,
			AgentName: agent,
			Status:    status,
		}); err != nil {
			t.Fatalf("InsertTask(%s): %v", id, err)
		}
		// Recency ordering in CountConsecutiveAgentFailures is by rowid DESC,
		// which is SQLite's implicit monotonic insert counter. That gives a
		// stable newest-first traversal even when multiple inserts land in the
		// same CURRENT_TIMESTAMP second.
	}

	// No tasks yet → 0.
	n, err := s.CountConsecutiveAgentFailures("owner/repo", 1, "review-agent")
	if err != nil {
		t.Fatalf("CountConsecutiveAgentFailures: %v", err)
	}
	if n != 0 {
		t.Fatalf("empty = %d, want 0", n)
	}

	// Chronological sequence for review-agent:
	//   1: failed, 2: failed, 3: completed, 4: failed, 5: timeout, 6: failed
	// Only the trailing 3 failures after task 3 should be counted.
	insert("t1", "review-agent", TaskStatusFailed)
	insert("t2", "review-agent", TaskStatusFailed)
	insert("t3", "review-agent", TaskStatusCompleted)
	insert("t4", "review-agent", TaskStatusFailed)
	insert("t5", "review-agent", TaskStatusTimeout)
	insert("t6", "review-agent", TaskStatusFailed)

	// Unrelated agent and pending rows must not affect the count.
	insert("t7", "dev-agent", TaskStatusFailed)
	insert("t8", "review-agent", TaskStatusPending)
	insert("t9", "review-agent", TaskStatusRunning)

	n, err = s.CountConsecutiveAgentFailures("owner/repo", 1, "review-agent")
	if err != nil {
		t.Fatalf("CountConsecutiveAgentFailures: %v", err)
	}
	if n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}

	// Filters by repo + issue + agent.
	n, err = s.CountConsecutiveAgentFailures("owner/repo", 1, "dev-agent")
	if err != nil {
		t.Fatalf("CountConsecutiveAgentFailures(dev-agent): %v", err)
	}
	if n != 1 {
		t.Fatalf("dev-agent count = %d, want 1", n)
	}
	n, err = s.CountConsecutiveAgentFailures("owner/repo", 2, "review-agent")
	if err != nil {
		t.Fatalf("CountConsecutiveAgentFailures(issue 2): %v", err)
	}
	if n != 0 {
		t.Fatalf("other-issue count = %d, want 0", n)
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

func TestWorkerTokenLifecycle(t *testing.T) {
	s := newTestStore(t)

	issued, err := s.IssueWorkerToken("worker-1", "owner/repo", []string{"dev"}, "host1")
	if err != nil {
		t.Fatalf("IssueWorkerToken: %v", err)
	}
	if issued.KID == "" || issued.Token == "" {
		t.Fatalf("issued token missing fields: %+v", issued)
	}

	auth, err := s.AuthenticateWorkerToken(issued.Token)
	if err != nil {
		t.Fatalf("AuthenticateWorkerToken: %v", err)
	}
	if auth.WorkerID != "worker-1" || auth.KID != issued.KID {
		t.Fatalf("unexpected auth record: %+v", auth)
	}

	listed, err := s.ListWorkerTokens("owner/repo")
	if err != nil {
		t.Fatalf("ListWorkerTokens: %v", err)
	}
	if len(listed) != 1 || listed[0].WorkerID != "worker-1" || listed[0].KID != issued.KID {
		t.Fatalf("unexpected token records: %+v", listed)
	}
	if listed[0].RevokedAt != nil {
		t.Fatalf("token should be active: %+v", listed[0])
	}

	if err := s.RevokeWorkerToken("worker-1", issued.KID); err != nil {
		t.Fatalf("RevokeWorkerToken: %v", err)
	}
	if _, err := s.AuthenticateWorkerToken(issued.Token); err == nil {
		t.Fatal("expected revoked token auth to fail")
	}

	listed, err = s.ListWorkerTokens("owner/repo")
	if err != nil {
		t.Fatalf("ListWorkerTokens after revoke: %v", err)
	}
	if listed[0].RevokedAt == nil {
		t.Fatalf("expected revoked timestamp after revoke: %+v", listed[0])
	}
}

func TestDeleteWorker(t *testing.T) {
	s := newTestStore(t)

	if err := s.InsertWorker(WorkerRecord{ID: "w-del", Repo: "org/repo", Roles: `["dev"]`, Hostname: "host1", Status: "online"}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	deleted, err := s.DeleteWorker("w-del")
	if err != nil {
		t.Fatalf("DeleteWorker: %v", err)
	}
	if !deleted {
		t.Fatal("expected worker to be deleted")
	}

	workers, err := s.QueryWorkers("")
	if err != nil {
		t.Fatalf("QueryWorkers: %v", err)
	}
	if len(workers) != 0 {
		t.Fatalf("expected 0 workers after delete, got %d", len(workers))
	}

	// Deleting a non-existent worker should return false without error.
	deleted, err = s.DeleteWorker("w-missing")
	if err != nil {
		t.Fatalf("DeleteWorker non-existent: %v", err)
	}
	if deleted {
		t.Fatal("expected deleted=false for missing worker")
	}
}

func TestInsertWorkerReRegistrationBumpsHeartbeat(t *testing.T) {
	s := newTestStore(t)

	if err := s.InsertWorker(WorkerRecord{ID: "w-rr", Repo: "org/repo", Roles: `["dev"]`, Hostname: "h1", Status: "online"}); err != nil {
		t.Fatalf("InsertWorker initial: %v", err)
	}
	first, err := s.GetWorker("w-rr")
	if err != nil || first == nil {
		t.Fatalf("GetWorker initial: %v %+v", err, first)
	}

	// Force a stale heartbeat so we can prove re-registration refreshes it.
	if _, err := s.DB().Exec(`UPDATE workers SET last_heartbeat = datetime('now', '-1 hour') WHERE id = 'w-rr'`); err != nil {
		t.Fatalf("force stale heartbeat: %v", err)
	}
	stale, err := s.GetWorker("w-rr")
	if err != nil || stale == nil {
		t.Fatalf("GetWorker stale: %v %+v", err, stale)
	}

	if err := s.InsertWorker(WorkerRecord{ID: "w-rr", Repo: "org/repo", Roles: `["dev"]`, Hostname: "h1", Status: "online"}); err != nil {
		t.Fatalf("InsertWorker re-register: %v", err)
	}
	after, err := s.GetWorker("w-rr")
	if err != nil || after == nil {
		t.Fatalf("GetWorker after: %v %+v", err, after)
	}
	if !after.LastHeartbeat.After(stale.LastHeartbeat) {
		t.Fatalf("expected last_heartbeat to advance after re-registration; stale=%v after=%v", stale.LastHeartbeat, after.LastHeartbeat)
	}
}

func TestWorkerHasRunningTask(t *testing.T) {
	s := newTestStore(t)

	if err := s.InsertWorker(WorkerRecord{ID: "w-run", Repo: "org/repo", Roles: `["dev"]`, Hostname: "host1", Status: "online"}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	hasTask, err := s.WorkerHasRunningTask("w-run")
	if err != nil {
		t.Fatalf("WorkerHasRunningTask: %v", err)
	}
	if hasTask {
		t.Fatal("expected no running task")
	}

	if err := s.InsertTask(TaskRecord{ID: "task-run", Repo: "org/repo", IssueNum: 1, AgentName: "dev", Status: TaskStatusPending}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	claimed, err := s.ClaimTask("task-run", "w-run")
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim to succeed")
	}

	hasTask, err = s.WorkerHasRunningTask("w-run")
	if err != nil {
		t.Fatalf("WorkerHasRunningTask after claim: %v", err)
	}
	if !hasTask {
		t.Fatal("expected running task to be detected")
	}
}

// TestAcquireIssueClaimFresh verifies a first-time acquisition returns a
// non-empty token and persists an issue_claim row (AC-2, AC-8).
func TestAcquireIssueClaimFresh(t *testing.T) {
	s := newTestStore(t)

	res, err := s.AcquireIssueClaim("org/repo", 42, "worker-a", time.Minute)
	if err != nil {
		t.Fatalf("AcquireIssueClaim fresh: %v", err)
	}
	if res.ClaimToken == "" {
		t.Fatal("expected non-empty claim token")
	}
	if res.Extended || res.OverwrotePrior {
		t.Fatalf("unexpected flags on fresh acquire: %+v", res)
	}

	got, err := s.QueryIssueClaim("org/repo", 42)
	if err != nil {
		t.Fatalf("QueryIssueClaim: %v", err)
	}
	if got == nil {
		t.Fatal("expected persisted claim")
	}
	if got.WorkerID != "worker-a" || got.ClaimToken != res.ClaimToken {
		t.Fatalf("unexpected claim: %+v", got)
	}
	if !got.ExpiresAt.After(time.Now().UTC().Add(30 * time.Second)) {
		t.Fatalf("expected expires_at well into the future, got %v", got.ExpiresAt)
	}
}

// TestAcquireIssueClaimSelfExtend verifies that the same worker acquiring an
// already-held claim extends the existing lease in place (AC-2, AC-8).
func TestAcquireIssueClaimSelfExtend(t *testing.T) {
	s := newTestStore(t)
	clock := newFakeClock(time.Date(2026, time.April, 18, 12, 0, 0, 0, time.UTC))
	s.SetNowFunc(clock.Now)

	first, err := s.AcquireIssueClaim("org/repo", 7, "worker-a", 30*time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	before, err := s.QueryIssueClaim("org/repo", 7)
	if err != nil {
		t.Fatalf("QueryIssueClaim before extend: %v", err)
	}
	if before == nil {
		t.Fatal("claim disappeared before extend")
	}

	clock.Advance(time.Second)

	second, err := s.AcquireIssueClaim("org/repo", 7, "worker-a", 5*time.Minute)
	if err != nil {
		t.Fatalf("self re-acquire: %v", err)
	}
	if !second.Extended {
		t.Fatalf("expected Extended=true, got %+v", second)
	}
	if second.ClaimToken != first.ClaimToken {
		t.Fatalf("self-extend should reuse the token: first=%q second=%q", first.ClaimToken, second.ClaimToken)
	}
	got, err := s.QueryIssueClaim("org/repo", 7)
	if err != nil {
		t.Fatalf("QueryIssueClaim: %v", err)
	}
	if got == nil {
		t.Fatal("claim disappeared after extend")
	}
	if !got.ExpiresAt.After(before.ExpiresAt) {
		t.Fatalf("expected extended expires_at to increase; before=%v after=%v", before.ExpiresAt, got.ExpiresAt)
	}
}

// TestAcquireIssueClaimHeldByOther verifies contention returns the sentinel
// error and does not modify the persisted claim (AC-2, AC-8).
func TestAcquireIssueClaimHeldByOther(t *testing.T) {
	s := newTestStore(t)

	first, err := s.AcquireIssueClaim("org/repo", 9, "worker-a", 5*time.Minute)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	_, err = s.AcquireIssueClaim("org/repo", 9, "worker-b", 5*time.Minute)
	if !errors.Is(err, ErrIssueClaimHeldByOther) {
		t.Fatalf("expected ErrIssueClaimHeldByOther, got %v", err)
	}

	got, err := s.QueryIssueClaim("org/repo", 9)
	if err != nil {
		t.Fatalf("QueryIssueClaim: %v", err)
	}
	if got == nil {
		t.Fatal("expected existing claim row")
	}
	if got.WorkerID != "worker-a" || got.ClaimToken != first.ClaimToken {
		t.Fatalf("claim should still belong to worker-a with original token, got %+v", got)
	}
}

// TestAcquireIssueClaimOverwritesExpired verifies an expired claim is
// transparently overwritten by a new acquirer (AC-6, AC-8).
func TestAcquireIssueClaimOverwritesExpired(t *testing.T) {
	s := newTestStore(t)
	clock := newFakeClock(time.Date(2026, time.April, 18, 12, 0, 0, 0, time.UTC))
	s.SetNowFunc(clock.Now)

	if _, err := s.AcquireIssueClaim("org/repo", 11, "worker-a", time.Second); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	clock.Advance(2 * time.Second)

	res, err := s.AcquireIssueClaim("org/repo", 11, "worker-b", 5*time.Minute)
	if err != nil {
		t.Fatalf("expired re-acquire: %v", err)
	}
	if !res.OverwrotePrior {
		t.Fatalf("expected OverwrotePrior=true, got %+v", res)
	}
	if res.PriorWorkerID != "worker-a" {
		t.Fatalf("expected PriorWorkerID=worker-a, got %q", res.PriorWorkerID)
	}
	if res.ClaimToken == "" {
		t.Fatal("expected non-empty replacement token")
	}
	got, err := s.QueryIssueClaim("org/repo", 11)
	if err != nil {
		t.Fatalf("QueryIssueClaim: %v", err)
	}
	if got == nil {
		t.Fatal("expected current claim row")
	}
	if got.WorkerID != "worker-b" || got.ClaimToken != res.ClaimToken {
		t.Fatalf("claim should now belong to worker-b with new token, got %+v", got)
	}
}

// TestReleaseIssueClaim verifies release only succeeds for the holder (AC-3).
func TestReleaseIssueClaim(t *testing.T) {
	s := newTestStore(t)

	res, err := s.AcquireIssueClaim("org/repo", 5, "worker-a", 5*time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Non-holder release is a no-op.
	ok, err := s.ReleaseIssueClaim("org/repo", 5, "worker-b", res.ClaimToken)
	if err != nil {
		t.Fatalf("non-holder release: %v", err)
	}
	if ok {
		t.Fatal("expected non-holder release to return false")
	}

	// Wrong token is also a no-op.
	ok, err = s.ReleaseIssueClaim("org/repo", 5, "worker-a", "wrong-token")
	if err != nil {
		t.Fatalf("wrong-token release: %v", err)
	}
	if ok {
		t.Fatal("expected wrong-token release to return false")
	}

	// Holder release with the matching token succeeds.
	ok, err = s.ReleaseIssueClaim("org/repo", 5, "worker-a", res.ClaimToken)
	if err != nil {
		t.Fatalf("holder release: %v", err)
	}
	if !ok {
		t.Fatal("expected holder release to return true")
	}

	got, err := s.QueryIssueClaim("org/repo", 5)
	if err != nil {
		t.Fatalf("QueryIssueClaim: %v", err)
	}
	if got != nil {
		t.Fatalf("expected claim to be deleted, got %+v", got)
	}

	// Releasing a missing claim returns false, not an error.
	ok, err = s.ReleaseIssueClaim("org/repo", 5, "worker-a", res.ClaimToken)
	if err != nil {
		t.Fatalf("release after delete: %v", err)
	}
	if ok {
		t.Fatal("expected release of missing claim to return false")
	}
}

// TestRefreshIssueClaim verifies refresh extends the holder's lease and is a
// no-op for other workers or expired claims (AC-4).
func TestRefreshIssueClaim(t *testing.T) {
	s := newTestStore(t)
	clock := newFakeClock(time.Date(2026, time.April, 18, 12, 0, 0, 0, time.UTC))
	s.SetNowFunc(clock.Now)

	res, err := s.AcquireIssueClaim("org/repo", 14, "worker-a", 30*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	before, err := s.QueryIssueClaim("org/repo", 14)
	if err != nil {
		t.Fatalf("QueryIssueClaim before: %v", err)
	}

	clock.Advance(time.Second)

	ok, err := s.RefreshIssueClaim("org/repo", 14, "worker-a", res.ClaimToken, 10*time.Minute)
	if err != nil {
		t.Fatalf("RefreshIssueClaim: %v", err)
	}
	if !ok {
		t.Fatal("expected refresh to succeed for holder")
	}
	after, err := s.QueryIssueClaim("org/repo", 14)
	if err != nil {
		t.Fatalf("QueryIssueClaim after: %v", err)
	}
	if !after.ExpiresAt.After(before.ExpiresAt) {
		t.Fatalf("expected extended expires_at; before=%v after=%v", before.ExpiresAt, after.ExpiresAt)
	}

	// Non-holder refresh is a no-op.
	ok, err = s.RefreshIssueClaim("org/repo", 14, "worker-b", res.ClaimToken, 10*time.Minute)
	if err != nil {
		t.Fatalf("non-holder refresh: %v", err)
	}
	if ok {
		t.Fatal("expected non-holder refresh to return false")
	}

	// Wrong token is also a no-op.
	ok, err = s.RefreshIssueClaim("org/repo", 14, "worker-a", "wrong-token", 10*time.Minute)
	if err != nil {
		t.Fatalf("wrong-token refresh: %v", err)
	}
	if ok {
		t.Fatal("expected wrong-token refresh to return false")
	}

	// Expired self-claim cannot be refreshed (the claim must be re-acquired).
	short, err := s.AcquireIssueClaim("org/repo", 15, "worker-a", time.Second)
	if err != nil {
		t.Fatalf("acquire short: %v", err)
	}
	clock.Advance(2 * time.Second)
	ok, err = s.RefreshIssueClaim("org/repo", 15, "worker-a", short.ClaimToken, 10*time.Minute)
	if err != nil {
		t.Fatalf("refresh expired: %v", err)
	}
	if ok {
		t.Fatal("expected refresh of expired self-claim to return false")
	}
}

// TestAcquireIssueClaimConcurrent exercises the primary-key conflict path
// in AcquireIssueClaim: N goroutines racing on the same (repo, issueNum)
// with distinct worker IDs must produce exactly one ClaimToken winner and
// N-1 ErrIssueClaimHeldByOther results. Without the PK-conflict handler the
// losers surface a raw "UNIQUE constraint failed" error.
func TestAcquireIssueClaimConcurrent(t *testing.T) {
	s := newTestStore(t)

	const goroutines = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		wins     int
		conflict int
		other    []error
	)
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		worker := fmt.Sprintf("worker-%d", i)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.AcquireIssueClaim("org/repo", 77, worker, 5*time.Minute)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrIssueClaimHeldByOther):
				conflict++
			default:
				other = append(other, err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("expected exactly one winner, got %d (conflicts=%d other=%v)", wins, conflict, other)
	}
	if len(other) > 0 {
		t.Fatalf("unexpected errors from racing acquirers (conflicts=%d wins=%d): %v", conflict, wins, other)
	}
	if conflict != goroutines-1 {
		t.Fatalf("expected %d conflicts, got %d", goroutines-1, conflict)
	}
}

// Regression test for the #141/#143 late-submit bug: once a task is in a
// terminal status, a coordinator submit path must not silently overwrite
// it back to a non-terminal status. TransitionTaskStatusIfRunning returns
// ErrTaskStatusTerminal so the caller can return an explicit 409.
func TestTransitionTaskStatusIfRunningRejectsTerminal(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertTask(TaskRecord{
		ID:        "task-terminal",
		Repo:      "org/repo",
		IssueNum:  101,
		AgentName: "dev-agent",
		Role:      "dev",
		Status:    TaskStatusRunning,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	// Normal transition: running → completed must succeed.
	if err := s.TransitionTaskStatusIfRunning("task-terminal", TaskStatusCompleted); err != nil {
		t.Fatalf("first transition failed: %v", err)
	}

	// Second attempt: completed is terminal; must be rejected with the
	// sentinel so the coordinator returns 409 instead of overwriting.
	err := s.TransitionTaskStatusIfRunning("task-terminal", TaskStatusFailed)
	if !errors.Is(err, ErrTaskStatusTerminal) {
		t.Fatalf("expected ErrTaskStatusTerminal for terminal task, got %v", err)
	}

	// The status must remain completed — the zombie submit must not have
	// overwritten it.
	got, err := s.GetTask("task-terminal")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != TaskStatusCompleted {
		t.Fatalf("status overwritten after terminal rejection: got %q, want %q", got.Status, TaskStatusCompleted)
	}

	// Unknown task must still surface a clear error (not the sentinel).
	if err := s.TransitionTaskStatusIfRunning("missing", TaskStatusFailed); err == nil || errors.Is(err, ErrTaskStatusTerminal) {
		t.Fatalf("expected not-found error, got %v", err)
	}
}
