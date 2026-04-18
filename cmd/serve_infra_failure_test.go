package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestExecuteTask_InfraFailureSkipsMarkAgentCompleted covers issue #131 /
// AC-3 and AC-4 end-to-end through the single-process worker path:
//
//  1. When the launcher returns a Result with Meta[infra_failure]="true",
//     executeTask must NOT call StateMachine.MarkAgentCompleted with a
//     failure signal. We detect that by asserting no eventlog.TypeCompleted
//     event was recorded (MarkAgentCompleted always emits one).
//  2. executeTask must emit an eventlog.TypeInfraFailure event so
//     operators can distinguish these from FAIL verdicts.
//  3. A report comment must still be posted (AC-3), and its body must
//     use the distinct "Infra Error" header (AC-2).
//
// The task's store status is still updated to failed so REQ-055's
// dispatch failure cap (#130) continues to bound retries.
func TestExecuteTask_InfraFailureSkipsMarkAgentCompleted(t *testing.T) {
	rt := &mockRuntime{
		name: config.RuntimeClaudeCode,
		resultFn: func(_ context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
			// Simulate a pre-LLM launcher crash: the runtime returns a
			// Result marked as infra_failure along with an error.
			return &launcher.Result{
				ExitCode: -1,
				Stderr:   "codex: thread main panicked at plugin-cache",
				Duration: 42 * time.Millisecond,
				Meta: map[string]string{
					launcher.MetaInfraFailure:       "true",
					launcher.MetaInfraFailureReason: "codex runtime panic/abort before agent output",
				},
			}, nil
		},
	}
	deps, st, comments := newWorkerTestDepsWithComments(t, rt)

	task := newWorkerTestTask(t, st, "owner/repo", 131, "task-infra-1")
	executeTask(context.Background(), task, deps)

	// Task is still marked as failed so REQ-055 (#130) dispatch cap can
	// count it toward runaway-loop bounds.
	statuses := taskStatusesByID(t, st)
	if statuses["task-infra-1"] != store.TaskStatusFailed {
		t.Fatalf("task status = %q, want %q", statuses["task-infra-1"], store.TaskStatusFailed)
	}

	// TypeInfraFailure event MUST be present (AC-4).
	events, err := st.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	var sawInfra, sawCompleted bool
	for _, e := range events {
		switch e.Type {
		case eventlog.TypeInfraFailure:
			sawInfra = true
		case eventlog.TypeCompleted:
			// MarkAgentCompleted always emits TypeCompleted; its absence
			// is our proof that we did NOT call it.
			sawCompleted = true
		}
	}
	if !sawInfra {
		t.Fatalf("expected eventlog.TypeInfraFailure event, got events=%v", events)
	}
	if sawCompleted {
		t.Fatalf("infra failure must NOT call StateMachine.MarkAgentCompleted (which would emit TypeCompleted); events=%v", events)
	}

	// A report comment was still posted (AC-3), and it uses the distinct
	// Infra Error header (AC-2).
	allComments := comments.Comments()
	if len(allComments) != 2 {
		t.Fatalf("expected started + infra-error report comments, got %d", len(allComments))
	}
	infraBody := allComments[1]
	if !strings.Contains(infraBody, "Infra Error") {
		t.Fatalf("expected 'Infra Error' in report body: %s", infraBody)
	}
	if !strings.Contains(infraBody, "not an agent verdict") {
		t.Fatalf("expected 'not an agent verdict' disclaimer in body: %s", infraBody)
	}
}

// TestExecuteTask_InfraFailureFromLauncherStartError verifies the
// launcher.Start() error path in executeTask is also classified as an
// infra failure rather than an agent verdict. This mirrors a common
// production case: the prompt template references a field that cannot
// render.
func TestExecuteTask_InfraFailureFromLauncherStartError(t *testing.T) {
	rt := &mockRuntime{
		name: config.RuntimeClaudeCode,
		resultFn: func(_ context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
			t.Fatal("runtime Run should not be called when Start fails")
			return nil, nil
		},
	}
	// Make Start fail by installing a dedicated mock; the existing
	// mockRuntime.Start always succeeds, so we wrap it.
	failStartRT := &startFailRuntime{name: config.RuntimeClaudeCode, wrapped: rt}
	deps, st, comments := newWorkerTestDepsWithComments(t, rt)
	// Re-register so the launcher uses the failStartRT for claude-code.
	deps.launcher.Register(failStartRT, config.RuntimeClaudeCode, config.RuntimeClaudeShot)

	task := newWorkerTestTask(t, st, "owner/repo", 132, "task-start-fail")
	executeTask(context.Background(), task, deps)

	statuses := taskStatusesByID(t, st)
	if statuses["task-start-fail"] != store.TaskStatusFailed {
		t.Fatalf("task status = %q, want %q", statuses["task-start-fail"], store.TaskStatusFailed)
	}

	// TypeInfraFailure event MUST be emitted, TypeCompleted MUST NOT.
	events, err := st.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	var sawInfra, sawCompleted bool
	for _, e := range events {
		switch e.Type {
		case eventlog.TypeInfraFailure:
			sawInfra = true
		case eventlog.TypeCompleted:
			sawCompleted = true
		}
	}
	if !sawInfra {
		t.Fatalf("expected TypeInfraFailure event on launcher.Start failure, got events=%v", events)
	}
	if sawCompleted {
		t.Fatalf("launcher.Start failure must not trigger MarkAgentCompleted; events=%v", events)
	}

	allComments := comments.Comments()
	if len(allComments) == 0 {
		t.Fatalf("expected at least one comment on infra failure")
	}
	lastBody := allComments[len(allComments)-1]
	if !strings.Contains(lastBody, "Infra Error") {
		t.Fatalf("expected Infra Error rendering, got: %s", lastBody)
	}
}

// TestExecuteTask_GenuineFailureCallsMarkAgentCompleted is the
// backwards-compatibility counterpart of the tests above: a genuine
// agent FAIL verdict (non-zero exit, no infra_failure meta) MUST still
// flow through MarkAgentCompleted so the state machine can act on the
// verdict. Without this guard we would regress behaviour for the common
// dev-agent-failed case.
func TestExecuteTask_GenuineFailureCallsMarkAgentCompleted(t *testing.T) {
	rt := &mockRuntime{
		name: config.RuntimeClaudeCode,
		resultFn: func(_ context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
			return &launcher.Result{
				ExitCode: 2,
				Stderr:   "tests failed",
				Duration: 50 * time.Millisecond,
				Meta:     map[string]string{},
			}, nil
		},
	}
	deps, st, _ := newWorkerTestDepsWithComments(t, rt)

	task := newWorkerTestTask(t, st, "owner/repo", 133, "task-genuine-fail")
	executeTask(context.Background(), task, deps)

	statuses := taskStatusesByID(t, st)
	if statuses["task-genuine-fail"] != store.TaskStatusFailed {
		t.Fatalf("task status = %q, want %q", statuses["task-genuine-fail"], store.TaskStatusFailed)
	}

	// TypeCompleted MUST be emitted (MarkAgentCompleted ran). TypeInfraFailure
	// must NOT.
	events, err := st.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	var sawInfra, sawCompleted bool
	for _, e := range events {
		switch e.Type {
		case eventlog.TypeInfraFailure:
			sawInfra = true
		case eventlog.TypeCompleted:
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Fatalf("genuine FAIL verdict MUST emit TypeCompleted via MarkAgentCompleted; events=%v", events)
	}
	if sawInfra {
		t.Fatalf("genuine FAIL verdict must NOT emit TypeInfraFailure; events=%v", events)
	}
}

// startFailRuntime wraps a runtime but forces Start() to return an
// error — used to exercise the launcher.Start error path in
// executeTask. The error string is crafted to match what a real
// template-render failure would produce, so the infra_failure reason
// string is still useful for operators.
type startFailRuntime struct {
	name    string
	wrapped *mockRuntime
}

func (s *startFailRuntime) Name() string { return s.name }

func (s *startFailRuntime) Start(context.Context, *config.AgentConfig, *launcher.TaskContext) (launcher.Session, error) {
	return nil, &startFailError{msg: "template render: undefined field Issue.Missing"}
}

func (s *startFailRuntime) Launch(context.Context, *config.AgentConfig, *launcher.TaskContext) (*launcher.Result, error) {
	return nil, &startFailError{msg: "template render: undefined field Issue.Missing"}
}

type startFailError struct {
	msg string
}

func (e *startFailError) Error() string { return e.msg }
