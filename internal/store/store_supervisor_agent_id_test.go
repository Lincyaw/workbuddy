package store

import "testing"

// TestUpdateTaskSupervisorAgentID covers REQ-061 / issue #234: the
// task_queue row gains a nullable supervisor_agent_id column that the
// worker writes immediately after the supervisor IPC accepts /agents.
// Old rows (no agent id yet) read back as the empty string; once written
// the value round-trips through Get/QueryTasks.
func TestUpdateTaskSupervisorAgentID(t *testing.T) {
	s := newTestStore(t)

	if err := s.InsertTask(TaskRecord{
		ID:        "task-sup-1",
		Repo:      "org/repo",
		IssueNum:  42,
		AgentName: "dev",
		Status:    TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	got, err := s.GetTask("task-sup-1")
	if err != nil {
		t.Fatalf("GetTask before set: %v", err)
	}
	if got.SupervisorAgentID != "" {
		t.Fatalf("fresh task SupervisorAgentID = %q, want empty", got.SupervisorAgentID)
	}

	if err := s.UpdateTaskSupervisorAgentID("task-sup-1", "agent-uuid-abc"); err != nil {
		t.Fatalf("UpdateTaskSupervisorAgentID: %v", err)
	}

	got, err = s.GetTask("task-sup-1")
	if err != nil {
		t.Fatalf("GetTask after set: %v", err)
	}
	if got.SupervisorAgentID != "agent-uuid-abc" {
		t.Fatalf("SupervisorAgentID = %q, want %q", got.SupervisorAgentID, "agent-uuid-abc")
	}

	// Unknown task id is a hard error rather than a silent no-op.
	if err := s.UpdateTaskSupervisorAgentID("no-such-task", "x"); err == nil {
		t.Fatal("UpdateTaskSupervisorAgentID(no-such-task): want error, got nil")
	}
}
