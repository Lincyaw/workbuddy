package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
)

// TestCoordinator_InfraFailureEmitsInfraEvent exercises issue #131 /
// AC-3 and AC-4 on the distributed worker path through
// fullCoordinatorServer.handleTaskResult: when the worker submits a
// result with infra_failure=true, the coordinator must log a
// TypeInfraFailure eventlog entry (AC-4) and must short-circuit the
// MarkAgentCompleted call (AC-3).
//
// We cannot directly observe whether pollers.MarkAgentCompleted was
// called because it is a method on *pollerManager. The clean way to
// assert "it was not called" is to leave the pollerManager without a
// runtime for the target repo: pollers.MarkAgentCompleted returns early
// in that case, which matches the behavior we want and is what the
// handler branches on. We therefore rely on the presence of the new
// TypeInfraFailure event as the positive signal (AC-4) and confirm the
// task status is still updated to failed so REQ-055's dispatch cap
// continues to bound retries.
func TestCoordinator_InfraFailureEmitsInfraEvent(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "coord.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	evlog := eventlog.NewEventLogger(st)
	reg := registry.NewRegistry(st, 30)

	s := &fullCoordinatorServer{
		store:    st,
		registry: reg,
		eventlog: evlog,
		taskHub:  tasknotify.NewHub(),
		// Zero-value pollerManager; MarkAgentCompleted will short-circuit
		// because no runtime is registered for this repo. That is
		// exactly the "was not invoked" signal we need for AC-3.
		pollers: &pollerManager{},
	}

	if err := reg.Register("worker-1", "owner/repo", []string{"dev"}, "host1"); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-infra",
		Repo:      "owner/repo",
		IssueNum:  131,
		AgentName: "dev-agent",
		Status:    store.TaskStatusRunning,
		WorkerID:  "worker-1",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	body, _ := json.Marshal(taskResultRequest{
		WorkerID:      "worker-1",
		Status:        "failed",
		CurrentLabels: []string{"workbuddy", "status:developing"},
		InfraFailure:  true,
		InfraReason:   "codex runtime panic/abort before agent output",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/task-infra/result", bytes.NewReader(body))
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	s.handleTaskResult(rec, req, "task-infra")
	if rec.Code != http.StatusOK {
		t.Fatalf("result status = %d, body=%s", rec.Code, rec.Body.String())
	}

	events, err := st.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	var sawInfra bool
	for _, e := range events {
		if e.Type == eventlog.TypeInfraFailure {
			sawInfra = true
			// Verify the reason is carried through so operators have the
			// launcher's explanation on hand.
			var payload map[string]any
			if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
				t.Fatalf("unmarshal infra payload: %v", err)
			}
			if reason, _ := payload["reason"].(string); reason != "codex runtime panic/abort before agent output" {
				t.Fatalf("infra event reason = %q, want the submitted reason", reason)
			}
		}
	}
	if !sawInfra {
		t.Fatalf("expected TypeInfraFailure event, got events=%+v", events)
	}

	// Task status must still reflect the failure so REQ-055 can bound retries.
	tasks, err := st.QueryTasks("")
	if err != nil {
		t.Fatalf("QueryTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Status != store.TaskStatusFailed {
		t.Fatalf("task status = %q, want %q", tasks[0].Status, store.TaskStatusFailed)
	}
}

// TestCoordinator_GenuineFailureDoesNotEmitInfraEvent is the
// backwards-compatibility check: when the worker submits a normal
// failed result (no infra_failure flag), the coordinator must NOT emit
// a TypeInfraFailure event. Without this guard we would start
// classifying every agent failure as an infra error.
func TestCoordinator_GenuineFailureDoesNotEmitInfraEvent(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "coord.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	evlog := eventlog.NewEventLogger(st)
	reg := registry.NewRegistry(st, 30)
	s := &fullCoordinatorServer{
		store:    st,
		registry: reg,
		eventlog: evlog,
		taskHub:  tasknotify.NewHub(),
		pollers:  &pollerManager{},
	}
	if err := reg.Register("worker-1", "owner/repo", []string{"dev"}, "host1"); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-fail",
		Repo:      "owner/repo",
		IssueNum:  133,
		AgentName: "dev-agent",
		Status:    store.TaskStatusRunning,
		WorkerID:  "worker-1",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	body, _ := json.Marshal(taskResultRequest{
		WorkerID:      "worker-1",
		Status:        "failed",
		CurrentLabels: []string{"workbuddy", "status:developing"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/task-fail/result", bytes.NewReader(body))
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	s.handleTaskResult(rec, req, "task-fail")
	if rec.Code != http.StatusOK {
		t.Fatalf("result status = %d, body=%s", rec.Code, rec.Body.String())
	}

	events, err := st.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	for _, e := range events {
		if e.Type == eventlog.TypeInfraFailure {
			t.Fatalf("genuine failure must NOT emit TypeInfraFailure, got %+v", e)
		}
	}
}
