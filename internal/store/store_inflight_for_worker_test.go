package store

import (
	"path/filepath"
	"testing"
)

// TestInFlightTasksForWorker covers issue #245: a stateless worker boot
// must list every running task it claimed (matching by worker_id) so it can
// re-attach to the supervisor-managed agents and resume the events stream.
// Tasks without a recorded supervisor_agent_id are skipped — there is no
// agent to attach to. EventsV1Path is computed from the latest session row.
func TestInFlightTasksForWorker(t *testing.T) {
	s := newTestStore(t)

	mustInsertRunningTask(t, s, "task-A", "worker-1", "agent-A", "/sess/A")
	mustInsertRunningTask(t, s, "task-B", "worker-1", "agent-B", "/sess/B")
	// Different worker — must not appear.
	mustInsertRunningTask(t, s, "task-C", "worker-2", "agent-C", "/sess/C")
	// Same worker but no supervisor_agent_id — must be skipped.
	if err := s.InsertTask(TaskRecord{ID: "task-D", Repo: "org/r", IssueNum: 4, AgentName: "dev"}); err != nil {
		t.Fatalf("InsertTask D: %v", err)
	}
	if _, err := s.ClaimTask("task-D", "worker-1"); err != nil {
		t.Fatalf("ClaimTask D: %v", err)
	}
	// Same worker, completed status — must not appear (status filter).
	mustInsertRunningTask(t, s, "task-E", "worker-1", "agent-E", "/sess/E")
	if err := s.UpdateTaskStatus("task-E", TaskStatusCompleted); err != nil {
		t.Fatalf("UpdateTaskStatus E: %v", err)
	}

	got, err := s.InFlightTasksForWorker("worker-1")
	if err != nil {
		t.Fatalf("InFlightTasksForWorker: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 in-flight tasks, got %d (%+v)", len(got), got)
	}

	byID := map[string]InFlightTaskForWorker{}
	for _, r := range got {
		byID[r.TaskID] = r
	}
	if rec, ok := byID["task-A"]; !ok {
		t.Fatal("task-A missing")
	} else {
		if rec.SupervisorAgentID != "agent-A" {
			t.Fatalf("task-A SupervisorAgentID = %q", rec.SupervisorAgentID)
		}
		if rec.EventsV1Path != filepath.Join("/sess/A", "events-v1.jsonl") {
			t.Fatalf("task-A EventsV1Path = %q", rec.EventsV1Path)
		}
		if rec.SessionID != "sess-A" {
			t.Fatalf("task-A SessionID = %q", rec.SessionID)
		}
	}
	if _, ok := byID["task-B"]; !ok {
		t.Fatal("task-B missing")
	}
	if _, ok := byID["task-C"]; ok {
		t.Fatal("task-C must not appear (different worker)")
	}
	if _, ok := byID["task-D"]; ok {
		t.Fatal("task-D must not appear (no supervisor_agent_id)")
	}
	if _, ok := byID["task-E"]; ok {
		t.Fatal("task-E must not appear (completed)")
	}

	// Other-worker query is independent.
	other, err := s.InFlightTasksForWorker("worker-2")
	if err != nil {
		t.Fatalf("InFlightTasksForWorker worker-2: %v", err)
	}
	if len(other) != 1 || other[0].TaskID != "task-C" {
		t.Fatalf("worker-2 result = %+v", other)
	}
}

func mustInsertRunningTask(t *testing.T, s *Store, taskID, workerID, agentID, dir string) {
	t.Helper()
	if err := s.InsertTask(TaskRecord{ID: taskID, Repo: "org/r", IssueNum: 1, AgentName: "dev"}); err != nil {
		t.Fatalf("InsertTask %s: %v", taskID, err)
	}
	if _, err := s.ClaimTask(taskID, workerID); err != nil {
		t.Fatalf("ClaimTask %s: %v", taskID, err)
	}
	if err := s.UpdateTaskSupervisorAgentID(taskID, agentID); err != nil {
		t.Fatalf("UpdateTaskSupervisorAgentID %s: %v", taskID, err)
	}
	sessID := "sess-" + taskID[len(taskID)-1:]
	if _, err := s.CreateSession(SessionRecord{
		SessionID: sessID,
		TaskID:    taskID,
		Repo:      "org/r",
		IssueNum:  1,
		AgentName: "dev",
		WorkerID:  workerID,
		Status:    TaskStatusRunning,
		Dir:       dir,
	}); err != nil {
		t.Fatalf("CreateSession %s: %v", taskID, err)
	}
}
