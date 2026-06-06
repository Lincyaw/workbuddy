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
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

// TestAgentMBridge_CLISuccess wires an AgentMBackend (pointed at the fake
// binary) into the bridge runtime and walks the full Start → Run → Result
// path for the normal CLI mode: exit 0, no RESULT: line, the trailing
// assistant text surfaced as LastMessage (which the reporter posts as a
// comment). No next_label is produced in this mode.
func TestAgentMBridge_CLISuccess(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:      agentmtest.ModeSuccess,
		FinalText: "All acceptance criteria met.",
	})
	rt := NewAgentMRuntime(func() (agent.Backend, error) {
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
		Name:     "dev-agent",
		Runtime:  config.RuntimeAgentM,
		Role:     "dev",
		Prompt:   "ship REQ-134",
		Scenario: "agent_env",
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
	if res.LastMessage != "All acceptance criteria met." {
		t.Fatalf("LastMessage = %q, want the trailing assistant text", res.LastMessage)
	}
	if IsInfraFailure(res) {
		t.Fatalf("clean exit-0 CLI run must not be infra failure, meta=%v", res.Meta)
	}
	if res.SessionPath == "" {
		t.Fatalf("expected SessionPath populated from captured stdout transcript")
	}
}

// TestAgentMBridge_ResultSuccess covers the coordinator-managed RESULT: line
// path (REQ-142/146 preserved): when AgentM emits a structured success line,
// the bridge surfaces next_label on Result.Meta as before.
func TestAgentMBridge_ResultSuccess(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeResultSuccess})
	rt := NewAgentMRuntime(func() (agent.Backend, error) {
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
		t.Fatalf("expected SessionPath populated")
	}
}

// TestResolvePrompt_AgentMSuppressesFooter asserts the transition footer
// (the `gh issue edit --add-label …` routing instructions) is appended for
// claude/codex runtimes but NOT for agentm — the pod agent cannot run gh and
// the coordinator-managed LabelWriter owns the transition.
func TestResolvePrompt_AgentMSuppressesFooter(t *testing.T) {
	task := &TaskContext{
		Repo:  "Lincyaw/workbuddy",
		Issue: IssueContext{Number: 319, Title: "test"},
	}
	task.SetWorkflowState("developing", "status:developing", map[string]string{
		"status:review": "reviewing",
	})

	body := "Implement the change."

	claudeCfg := &config.AgentConfig{Name: "dev", Runtime: config.RuntimeClaudeCode, Prompt: body}
	claudePrompt := resolvePrompt(claudeCfg, task)
	if !strings.Contains(claudePrompt, "gh issue edit") {
		t.Fatalf("claude prompt should keep the transition footer:\n%s", claudePrompt)
	}

	agentmCfg := &config.AgentConfig{Name: "dev", Runtime: config.RuntimeAgentM, Prompt: body}
	agentmPrompt := resolveAgentMPrompt(agentmCfg, task)
	if strings.Contains(agentmPrompt, "gh issue edit") {
		t.Fatalf("agentm prompt must NOT carry the gh-edit footer:\n%s", agentmPrompt)
	}
	if !strings.Contains(agentmPrompt, body) {
		t.Fatalf("agentm prompt should still contain the agent body:\n%s", agentmPrompt)
	}
}

// TestAgentMBridge_MalformedRESULT covers AC-1-2: a malformed RESULT line
// is classified as infra failure with failure_reason captured in
// Result.Meta so the reporter can surface it.
func TestAgentMBridge_MalformedRESULT(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeMalformedJSON})
	rt := NewAgentMRuntime(func() (agent.Backend, error) {
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
	rt := NewAgentMRuntime(func() (agent.Backend, error) {
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
	rt := NewAgentMRuntime(func() (agent.Backend, error) {
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
	}, map[string]string{}, nil)
	if _, ok := env[EnvDevContainerImage]; ok {
		t.Fatalf("dev_container_image must not leak into non-agentm runtime env, got %v", env)
	}

	env2 := injectAgentMEnv(&config.AgentConfig{
		Name:    "dev-agent",
		Runtime: config.RuntimeAgentM,
		// no DevContainerImage: AgentM falls back to its own default
	}, map[string]string{}, nil)
	if _, ok := env2[EnvDevContainerImage]; ok {
		t.Fatalf("empty dev_container_image must not be injected, got %v", env2)
	}
}

// TestAgentMBridge_SessionLogDurableAndNoLeak covers the resource-leak fix:
// when a session handle is wired (the production worker path), the bridge
// copies the agentm-native session log into the durable worker session dir
// and points SessionPath at the copy, and the backend's per-session temp dir
// is removed on Close() so successful dispatches don't leak /tmp.
func TestAgentMBridge_SessionLogDurableAndNoLeak(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeSuccess})
	rt := NewAgentMRuntime(func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})

	work := t.TempDir()
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	mgr := NewSessionManager(sessionsDir, nil)
	handle, err := mgr.Create(SessionCreateInput{SessionID: "test-session", Repo: "Lincyaw/workbuddy", IssueNum: 319})
	if err != nil {
		t.Fatalf("create managed session: %v", err)
	}

	task := &TaskContext{
		Repo:     "Lincyaw/workbuddy",
		WorkDir:  work,
		RepoRoot: work,
		Issue:    IssueContext{Number: 319, Title: "test"},
		Session:  SessionContext{ID: "test-session", TaskID: "task-1", Attempt: 1},
	}
	task.SetSessionHandle(handle)
	agentCfg := &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeAgentM, Role: "dev", Prompt: "ship it"}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := rt.Start(ctx, agentCfg, task)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Reach into the backend session to capture the temp dir before Close.
	bridgeSess, ok := sess.(*AgentMSession)
	if !ok {
		t.Fatalf("expected *AgentMSession, got %T", sess)
	}
	logExtractor := bridgeSess.Session.(interface{ SessionLogPath() string })

	ch := make(chan launcherevents.Event, 32)
	drained := make(chan struct{})
	go func() {
		for range ch {
		}
		close(drained)
	}()
	res, runErr := sess.Run(ctx, ch)
	close(ch)
	<-drained
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}

	tmpDir := filepath.Dir(logExtractor.SessionLogPath())

	// SessionPath must point at the durable copy in the session dir, not the
	// backend temp dir that Close() will delete.
	if res.SessionPath == "" {
		t.Fatalf("expected SessionPath populated")
	}
	if got := filepath.Dir(res.SessionPath); got != handle.Dir() {
		t.Fatalf("SessionPath %q not in durable session dir %q", res.SessionPath, handle.Dir())
	}
	if _, err := os.Stat(res.SessionPath); err != nil {
		t.Fatalf("durable session log missing: %v", err)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The durable copy survives Close; the backend temp dir does not.
	if _, err := os.Stat(res.SessionPath); err != nil {
		t.Fatalf("durable session log removed by Close: %v", err)
	}
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Fatalf("backend temp dir %q still exists after Close (err=%v): agentm leaks /tmp", tmpDir, err)
	}
}

// TestIssueTraceparent_Deterministic asserts that the trace_id portion of
// the W3C traceparent is deterministic for the same repo+issue, while the
// span_id differs between calls.
func TestIssueTraceparent_Deterministic(t *testing.T) {
	tp1 := issueTraceparent("LGU-SE-Internal/opentelemetry-demo", 42)
	tp2 := issueTraceparent("LGU-SE-Internal/opentelemetry-demo", 42)

	parts1 := strings.Split(tp1, "-")
	parts2 := strings.Split(tp2, "-")
	if len(parts1) != 4 || len(parts2) != 4 {
		t.Fatalf("traceparent format invalid: %q / %q", tp1, tp2)
	}

	// Same trace_id (deterministic from repo+issue)
	if parts1[1] != parts2[1] {
		t.Fatalf("same repo+issue must yield same trace_id: %q vs %q", parts1[1], parts2[1])
	}
	// Different span_id (random per dispatch)
	if parts1[2] == parts2[2] {
		t.Fatalf("span_id should differ between calls: both %q", parts1[2])
	}
	// W3C format: version=00, flags=01
	if parts1[0] != "00" || parts1[3] != "01" {
		t.Fatalf("expected version=00, flags=01, got %q-%q", parts1[0], parts1[3])
	}
	// Hex length: trace_id=32, span_id=16
	if len(parts1[1]) != 32 {
		t.Fatalf("trace_id should be 32 hex chars, got %d", len(parts1[1]))
	}
	if len(parts1[2]) != 16 {
		t.Fatalf("span_id should be 16 hex chars, got %d", len(parts1[2]))
	}
}

// TestIssueTraceparent_DifferentIssue asserts different issues produce
// different trace IDs.
func TestIssueTraceparent_DifferentIssue(t *testing.T) {
	tp1 := issueTraceparent("Lincyaw/workbuddy", 1)
	tp2 := issueTraceparent("Lincyaw/workbuddy", 2)

	traceID1 := strings.Split(tp1, "-")[1]
	traceID2 := strings.Split(tp2, "-")[1]
	if traceID1 == traceID2 {
		t.Fatalf("different issues must yield different trace IDs: both %q", traceID1)
	}
}

// TestRepoToSlug covers the slug conversion used for filesystem paths.
func TestRepoToSlug(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Lincyaw/workbuddy", "Lincyaw-workbuddy"},
		{"LGU-SE-Internal/opentelemetry-demo", "LGU-SE-Internal-opentelemetry-demo"},
		{"simple", "simple"},
	}
	for _, tc := range cases {
		got := repoToSlug(tc.in)
		if got != tc.want {
			t.Errorf("repoToSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestInjectAgentMEnv_ObservabilityDir asserts that AGENTM_OBSERVABILITY_DIR
// is set for AgentM runtime with a valid task, and not set otherwise.
func TestInjectAgentMEnv_ObservabilityDir(t *testing.T) {
	task := &TaskContext{
		Repo:  "LGU-SE-Internal/opentelemetry-demo",
		Issue: IssueContext{Number: 42},
	}

	// AgentM + valid task → observability dir set
	env := injectAgentMEnv(&config.AgentConfig{
		Runtime: config.RuntimeAgentM,
	}, map[string]string{}, task)
	want := "/var/lib/workbuddy/traces/LGU-SE-Internal-opentelemetry-demo/issue-42"
	if got := env[EnvAgentMObservabilityDir]; got != want {
		t.Fatalf("observability dir = %q, want %q", got, want)
	}

	// Non-AgentM runtime → not set
	env2 := injectAgentMEnv(&config.AgentConfig{
		Runtime: config.RuntimeClaudeCode,
	}, map[string]string{}, task)
	if _, ok := env2[EnvAgentMObservabilityDir]; ok {
		t.Fatalf("observability dir must not be set for non-agentm runtime")
	}

	// AgentM + nil task → not set
	env3 := injectAgentMEnv(&config.AgentConfig{
		Runtime: config.RuntimeAgentM,
	}, map[string]string{}, nil)
	if _, ok := env3[EnvAgentMObservabilityDir]; ok {
		t.Fatalf("observability dir must not be set when task is nil")
	}

	// AgentM + task with no issue → not set
	env4 := injectAgentMEnv(&config.AgentConfig{
		Runtime: config.RuntimeAgentM,
	}, map[string]string{}, &TaskContext{Repo: "foo/bar"})
	if _, ok := env4[EnvAgentMObservabilityDir]; ok {
		t.Fatalf("observability dir must not be set when issue number is 0")
	}

	// Already-set value is preserved (idempotent)
	env5 := injectAgentMEnv(&config.AgentConfig{
		Runtime: config.RuntimeAgentM,
	}, map[string]string{EnvAgentMObservabilityDir: "/custom"}, task)
	if got := env5[EnvAgentMObservabilityDir]; got != "/custom" {
		t.Fatalf("existing value should be preserved, got %q", got)
	}
}
