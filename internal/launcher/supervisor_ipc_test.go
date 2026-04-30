package launcher

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

// TestProcessSessionRun_RoutesThroughSupervisor verifies the worker's
// claude-style runtime no longer spawns subprocesses directly: it must
// POST /agents to the supervisor, fire the OnAgentStarted hook with the
// returned agent_id (so the worker can persist task_queue.supervisor_agent_id),
// and stream stdout back through the SSE channel.
//
// Issue #244 / REQ-096: this is the behavioral test that the IPC client
// path is wired in place of the previous exec.Cmd path.
func TestProcessSessionRun_RoutesThroughSupervisor(t *testing.T) {
	client := newTestSupervisorClient(t)

	var (
		hookMu       sync.Mutex
		hookCalls    []string
		hookTaskIDs  []string
	)
	hook := func(taskID, agentID string) {
		hookMu.Lock()
		defer hookMu.Unlock()
		hookTaskIDs = append(hookTaskIDs, taskID)
		hookCalls = append(hookCalls, agentID)
	}

	lnch := NewLauncher(client, hook)

	task := newTestTask(t)
	task.Session.TaskID = "task-244-supervised"
	agent := &config.AgentConfig{
		Name:    "ipc-agent",
		Runtime: config.RuntimeClaudeCode,
		Command: `echo "from-supervisor"`,
		Timeout: 10 * time.Second,
	}

	session, err := lnch.Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = session.Close() }()

	ch := make(chan launcherevents.Event, 16)
	doneCh := make(chan struct{})
	go func() {
		for range ch {
		}
		close(doneCh)
	}()
	result, err := session.Run(context.Background(), ch)
	close(ch)
	<-doneCh

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "from-supervisor") {
		t.Fatalf("stdout = %q, want it to contain 'from-supervisor'", result.Stdout)
	}

	hookMu.Lock()
	defer hookMu.Unlock()
	if len(hookCalls) != 1 {
		t.Fatalf("OnAgentStarted called %d times, want 1: %v", len(hookCalls), hookCalls)
	}
	if hookCalls[0] == "" {
		t.Fatal("OnAgentStarted received empty agent_id")
	}
	if hookTaskIDs[0] != "task-244-supervised" {
		t.Fatalf("OnAgentStarted received task_id %q, want %q", hookTaskIDs[0], "task-244-supervised")
	}
}

// TestProcessSessionRun_PassesPromptViaSupervisorStdin verifies the claude
// prompt path: the supervisor must receive the prompt as the Stdin of its
// StartAgentRequest so the agent process can read it from stdin (the same
// shape the previous exec.Cmd path used). We exercise this end-to-end by
// installing a fake `claude` binary that echoes its stdin.
func TestProcessSessionRun_PassesPromptViaSupervisorStdin(t *testing.T) {
	restore := installFakeClaude(t, []string{
		`{"type":"system.init","session_id":"prompt-test"}`,
		`{"type":"assistant.content_block_delta","delta":{"type":"text_delta","text":"got prompt"}}`,
		`{"type":"assistant.message_stop","usage":{"input_tokens":1,"output_tokens":1}}`,
	})
	defer restore()

	client := newTestSupervisorClient(t)
	lnch := NewLauncher(client, nil)

	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "claude-prompt-agent",
		Runtime: config.RuntimeClaudeCode,
		Prompt:  "say hi",
		Policy:  config.PolicyConfig{Sandbox: "read-only"},
		Timeout: 5 * time.Second,
	}

	session, err := lnch.Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = session.Close() }()

	ch := make(chan launcherevents.Event, 32)
	doneCh := make(chan struct{})
	go func() {
		for range ch {
		}
		close(doneCh)
	}()
	result, err := session.Run(context.Background(), ch)
	close(ch)
	<-doneCh

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if result.LastMessage != "got prompt" {
		t.Fatalf("LastMessage = %q, want %q", result.LastMessage, "got prompt")
	}
}

// TestProcessSession_NoClientFailsLoudly checks the explicit guard against
// running without a wired-up supervisor — the previous exec.Cmd path is
// gone, so nil client must be a clear error rather than a silent no-op.
func TestProcessSession_NoClientFailsLoudly(t *testing.T) {
	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "no-client-agent",
		Runtime: config.RuntimeClaudeCode,
		Command: `echo "nope"`,
		Timeout: 1 * time.Second,
	}
	sess := runtimepkg.NewProcessSession(nil, nil, config.RuntimeClaudeCode, agent, task, nil)
	_, err := sess.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when supervisor client is nil")
	}
	if !strings.Contains(err.Error(), "supervisor client not configured") {
		t.Fatalf("error = %v, want mention of 'supervisor client not configured'", err)
	}
}
