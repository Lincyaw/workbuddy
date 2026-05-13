package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm/agentmtest"
	"github.com/Lincyaw/workbuddy/internal/config"
)

// TestAgentMBridge_HappyPath wires an AgentMBackend (pointed at the fake
// binary) into the bridge runtime and walks the full Start → Run → Result
// path. This is the v0.5 host-exec happy-path covered by AC-1-1.
func TestAgentMBridge_HappyPath(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeSuccess})
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})

	work := t.TempDir()
	task := &TaskContext{
		Repo:     "Lincyaw/workbuddy",
		WorkDir:  work,
		RepoRoot: work,
		Issue:    IssueContext{Number: 319, Title: "test"},
		Session:  SessionContext{ID: "test-session", TaskID: "task-1", Attempt: 1},
	}
	agentCfg := &config.AgentConfig{
		Name:    "dev-agent",
		Runtime: config.RuntimeAgentM,
		Role:    "dev",
		Prompt:  "ship REQ-134",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := rt.Launch(ctx, agentCfg, task)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d", res.ExitCode)
	}
	if got := res.Meta["agentm_next_label"]; got != "status:review" {
		t.Fatalf("next_label meta = %q", got)
	}
	if res.SessionPath == "" {
		t.Fatalf("expected SessionPath populated from agentm session_log_path")
	}
}

// TestAgentMBridge_MalformedRESULT covers AC-1-2: a malformed RESULT line
// is classified as infra failure with failure_reason captured in
// Result.Meta so the reporter can surface it.
func TestAgentMBridge_MalformedRESULT(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeMalformedJSON})
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})

	work := t.TempDir()
	task := &TaskContext{
		Repo:    "Lincyaw/workbuddy",
		WorkDir: work,
		Issue:   IssueContext{Number: 319},
		Session: SessionContext{ID: "test-session"},
	}
	agentCfg := &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeAgentM}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := rt.Launch(ctx, agentCfg, task)
	if err == nil {
		t.Fatalf("expected error for malformed RESULT")
	}
	if res == nil {
		t.Fatalf("expected result even on failure")
	}
	if !IsInfraFailure(res) {
		t.Fatalf("expected infra failure marker, meta=%v", res.Meta)
	}
	reason := res.Meta[MetaInfraFailureReason]
	if !strings.Contains(reason, "invalid RESULT") && !strings.Contains(reason, "invalid result file") {
		t.Fatalf("failure_reason should mention invalid RESULT, got %q", reason)
	}
}

// TestAgentMBridge_TaskFailure covers the case where AgentM cleanly reports
// success=false. next_label MUST still be present and the reason MUST flow
// into Meta so the reporter comment carries it.
func TestAgentMBridge_TaskFailure(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:          agentmtest.ModeFailure,
		NextLabel:     "status:failed",
		FailureReason: "acceptance criteria not met",
	})
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})

	work := t.TempDir()
	task := &TaskContext{
		Repo: "Lincyaw/workbuddy", WorkDir: work,
		Issue:   IssueContext{Number: 319},
		Session: SessionContext{ID: "test-session"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := rt.Launch(ctx, &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeAgentM}, task)
	if err == nil {
		t.Fatalf("expected error when agentm reports failure")
	}
	if res.Meta["agentm_next_label"] != "status:failed" {
		t.Fatalf("next_label meta = %q", res.Meta["agentm_next_label"])
	}
	if res.Meta["agentm_failure_reason"] != "acceptance criteria not met" {
		t.Fatalf("failure_reason meta = %q", res.Meta["agentm_failure_reason"])
	}
	// Task failure is distinct from infra failure: this is a clean signal
	// from the agent, not a contract violation.
	if IsInfraFailure(res) {
		t.Fatalf("clean failure should NOT be infra-failure")
	}
}

// TestAgentMBridge_DevContainerImageEnv covers REQ-140 / issue #328 AC-1-1:
// when the agent config sets dev_container_image and runtime=agentm, the
// AgentM subprocess MUST receive AGENTM_AGENT_ENV_IMAGE in its env.
// workbuddy passes the image name only — AgentM owns the actual sandbox
// dispatch, so this is a pass-through assertion, not an end-to-end one.
func TestAgentMBridge_DevContainerImageEnv(t *testing.T) {
	envDump := filepath.Join(t.TempDir(), "env.dump")
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:        agentmtest.ModeSuccess,
		EnvDumpPath: envDump,
	})
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})

	work := t.TempDir()
	task := &TaskContext{
		Repo:     "Lincyaw/workbuddy",
		WorkDir:  work,
		RepoRoot: work,
		Issue:    IssueContext{Number: 328},
		Session:  SessionContext{ID: "test-session"},
	}
	agentCfg := &config.AgentConfig{
		Name:              "dev-agent",
		Runtime:           config.RuntimeAgentM,
		Role:              "dev",
		Prompt:            "ship REQ-140",
		DevContainerImage: "ghcr.io/lincyaw/workbuddy-dev:latest",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := rt.Launch(ctx, agentCfg, task); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	data, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	want := EnvDevContainerImage + "=ghcr.io/lincyaw/workbuddy-dev:latest"
	if !strings.Contains(string(data), want) {
		t.Fatalf("env dump missing %q; got:\n%s", want, data)
	}
}

// TestAgentMBridge_DevContainerImageNotInjectedForOtherRuntimes asserts the
// pass-through is gated on runtime=agentm; for hypothetical claude-code /
// codex agents that happen to carry the field (config validation warns but
// permits it) the env var MUST NOT appear, since only AgentM understands it.
func TestAgentMBridge_DevContainerImageNotInjectedForOtherRuntimes(t *testing.T) {
	env := injectAgentMEnv(&config.AgentConfig{
		Name:              "dev-agent",
		Runtime:           config.RuntimeClaudeCode,
		DevContainerImage: "ghcr.io/x:y",
	}, map[string]string{})
	if _, ok := env[EnvDevContainerImage]; ok {
		t.Fatalf("dev_container_image must not leak into non-agentm runtime env, got %v", env)
	}

	env2 := injectAgentMEnv(&config.AgentConfig{
		Name:    "dev-agent",
		Runtime: config.RuntimeAgentM,
		// no DevContainerImage: AgentM falls back to its own default
	}, map[string]string{})
	if _, ok := env2[EnvDevContainerImage]; ok {
		t.Fatalf("empty dev_container_image must not be injected, got %v", env2)
	}
}
