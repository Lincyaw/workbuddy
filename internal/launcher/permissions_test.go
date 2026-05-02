package launcher

import (
	"context"
	"encoding/json"
	"os/exec"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent/codex/codextest"
	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

func TestBuildScopedEnv_StripsHostGitHubTokenVariables(t *testing.T) {
	t.Setenv("WORKBUDDY_DEV_PAT", "scoped-gh-token")
	t.Setenv("GH_TOKEN", "host-gh-token")
	t.Setenv("GITHUB_TOKEN", "github-host-token")
	t.Setenv("GITHUB_OAUTH", "oauth-host-token")

	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name: "dev-agent",
		Role: "dev",
		Permissions: config.PermissionsConfig{
			GitHub: config.GitHubPermissionsConfig{
				Token: "WORKBUDDY_DEV_PAT",
			},
		},
	}
	env := buildScopedEnv(agent, task)
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_OAUTH"} {
		if key == "GH_TOKEN" {
			continue
		}
		if envHas(env, key) {
			t.Fatalf("unexpected inherited host token env %q", key)
		}
	}
	if got, ok := envGet(env, "GH_TOKEN"); !ok || got != "scoped-gh-token" {
		t.Fatalf("GH_TOKEN = %q, ok=%v, want scoped-gh-token", got, ok)
	}
}

func TestBuildScopedEnv_InjectsScopedTokenAndFallsBackToHost(t *testing.T) {
	t.Setenv("WORKBUDDY_DEV_PAT", "scoped-gh-token")
	t.Setenv("GH_TOKEN", "stale-host-token")

	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name: "dev-agent",
		Role: "dev",
		Permissions: config.PermissionsConfig{
			GitHub: config.GitHubPermissionsConfig{
				Token: "WORKBUDDY_DEV_PAT",
			},
		},
	}

	env := buildScopedEnv(agent, task)
	if got, ok := envGet(env, "GH_TOKEN"); !ok || got != "scoped-gh-token" {
		t.Fatalf("GH_TOKEN = %q, ok=%v, want scoped-gh-token", got, ok)
	}
	for _, key := range []string{"GITHUB_TOKEN", "GITHUB_OAUTH"} {
		if envHas(env, key) {
			t.Fatalf("unexpected inherited token env %q", key)
		}
	}
}

func TestBuildScopedEnv_FallsBackToHostTokenWhenScopedMissing(t *testing.T) {
	t.Setenv("GH_TOKEN", "host-gh-token")

	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name: "review-agent",
		Role: "review",
		Permissions: config.PermissionsConfig{
			GitHub: config.GitHubPermissionsConfig{
				Token: "MISSING_SCOPED_TOKEN_VAR",
			},
		},
	}

	env := buildScopedEnv(agent, task)
	if got, ok := envGet(env, "GH_TOKEN"); !ok || got != "host-gh-token" {
		t.Fatalf("GH_TOKEN = %q, ok=%v, want host-gh-token", got, ok)
	}
}

func TestEffectivePermissionsPayload_DoesNotExposeTokenValue(t *testing.T) {
	t.Setenv("SCALED_SCOPED_TOKEN", "secret-token-value")
	agent := &config.AgentConfig{
		Name: "test-agent",
		Role: "review",
		Permissions: config.PermissionsConfig{
			GitHub: config.GitHubPermissionsConfig{
				Token: "SCALED_SCOPED_TOKEN",
			},
			FS: config.FileSystemPermissionsConfig{
				Write: "none",
			},
			Resources: config.ResourceLimitsConfig{
				MaxMemoryMB:   1024,
				MaxCPUPercent: 80,
			},
		},
	}
	payload := effectivePermissionsPayload(agent)
	if payload.Agent != "test-agent" || payload.Role != "review" {
		t.Fatalf("agent/role payload = %+v, want test-agent/review", payload)
	}
	if payload.GitHub.Token != "SCALED_SCOPED_TOKEN" {
		t.Fatalf("github token key = %q, want SCALED_SCOPED_TOKEN", payload.GitHub.Token)
	}
	if payload.GitHub.Token == "secret-token-value" {
		t.Fatalf("github token value leaked: %q", payload.GitHub.Token)
	}
	if payload.GitHub.Source != "scoped" {
		t.Fatalf("github token source = %q, want scoped", payload.GitHub.Source)
	}
	if payload.FS.Write != "none" {
		t.Fatalf("fs.write = %q, want none", payload.FS.Write)
	}
	if payload.Resources.MaxMemoryMB != 1024 || payload.Resources.MaxCPUPercent != 80 {
		t.Fatalf("resources = %+v, want 1024/80", payload.Resources)
	}
}

func TestProcessSessionRun_EmitsPermissionEvent(t *testing.T) {
	t.Setenv("GH_TOKEN", "host-gh-token")
	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "process-agent",
		Role:    "dev",
		Runtime: config.RuntimeClaudeCode,
		Command: "echo process-run-ok",
		Timeout: 10 * time.Second,
		Policy:  config.PolicyConfig{Sandbox: "read-only"},
	}

	session := newTestProcessSession(t, config.RuntimeClaudeCode, agent, task, nil)
	events, result, err := collectSessionEvents(t, session)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	payload := firstPermissionPayload(t, events)
	if payload.Agent != "process-agent" || payload.Role != "dev" {
		t.Fatalf("agent/role payload = %+v", payload)
	}
	if payload.GitHub.Token != "GH_TOKEN" {
		t.Fatalf("permission token key = %q, want GH_TOKEN", payload.GitHub.Token)
	}
	if payload.GitHub.Source != "host" {
		t.Fatalf("permission token source = %q, want host", payload.GitHub.Source)
	}
	if payload.GitHub.Token == "host-gh-token" {
		t.Fatalf("permission event leaks token value")
	}
}

func TestCodexSessionRun_EmitsPermissionEvent(t *testing.T) {
	restore := installFakeCodex(t)
	defer restore()
	t.Setenv("GH_TOKEN", "host-gh-token")

	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Role:    "review",
		Runtime: config.RuntimeCodex,
		Prompt:  "Reply with exactly HELLO",
		Policy:  config.PolicyConfig{Sandbox: "read-only", Approval: "never"},
		Timeout: 10 * time.Second,
	}
	session, err := newTestLauncher(t).Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = session.Close() }()

	events, result, err := collectSessionEvents(t, session)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	payload := firstPermissionPayload(t, events)
	if payload.Agent != "codex-agent" || payload.Role != "review" {
		t.Fatalf("agent/role payload = %+v", payload)
	}
	if payload.GitHub.Token != "GH_TOKEN" {
		t.Fatalf("permission token key = %q, want GH_TOKEN", payload.GitHub.Token)
	}
}

func TestCodexSessionRun_DangerSandboxFlowsToThreadStart(t *testing.T) {
	// Pre-REQ-127 this test asserted the worker forked codex with the
	// top-level --dangerously-bypass-approvals-and-sandbox CLI flag. With
	// the WS-transport refactor the worker no longer forks codex —
	// `workbuddy supervisor` does, with the bypass flag set once at
	// supervisor startup (covered by cmd/supervisor_codex_sidecar_test.go).
	// The remaining worker-side assertion is the per-session sandbox
	// param threading: an agent with Sandbox=danger-full-access must
	// produce thread/start params with sandbox="danger-full-access".
	logPath := filepath.Join(t.TempDir(), "fake.log")
	srv := codextest.NewServer(t, codextest.Config{Mode: codextest.ModeComplete, LogPath: logPath})
	defer srv.Close()
	t.Setenv("WORKBUDDY_CODEX_URL", srv.URL)

	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Role:    "dev",
		Runtime: config.RuntimeCodex,
		Prompt:  "Reply with exactly HELLO",
		Policy:  config.PolicyConfig{Sandbox: "danger-full-access", Approval: "never"},
		Timeout: 10 * time.Second,
	}
	session, err := newTestLauncher(t).Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = session.Close() }()

	_, result, err := collectSessionEvents(t, session)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake codex log: %v", err)
	}
	var threadStart map[string]any
	for _, line := range splitNonEmptyLines(string(logData)) {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("unmarshal fake codex log line %q: %v", line, err)
		}
		if obj["method"] == "thread/start" {
			threadStart, _ = obj["params"].(map[string]any)
		}
	}
	if threadStart == nil {
		t.Fatal("missing thread/start request in fake codex log")
	}
	if got, _ := threadStart["sandbox"].(string); got != "danger-full-access" {
		t.Fatalf("thread/start sandbox = %q, want danger-full-access; params=%#v", got, threadStart)
	}
}

func TestCodexSessionRun_DangerFullAccessE2E_FileWrite(t *testing.T) {
	if os.Getenv("CODEX_E2E") != "1" {
		t.Skip("set CODEX_E2E=1 to run real Codex end-to-end test")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}

	workdir := t.TempDir()
	filename := "wb_e2e_sandbox_check.txt"
	expected := "SANDBOX_OK\n"

	task := newTestTask(t)
	task.RepoRoot = workdir
	task.WorkDir = workdir

	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Role:    "dev",
		Runtime: config.RuntimeCodex,
		Prompt: "Use your tools to create a file named " + filename + " in the current working directory with exactly the contents SANDBOX_OK followed by a newline. " +
			"Then read the file back to confirm it and reply with E2E_OK plus the filename on one line.",
		Policy:  config.PolicyConfig{Sandbox: "danger-full-access", Approval: "never"},
		Timeout: 2 * time.Minute,
	}
	session, err := newTestLauncher(t).Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = session.Close() }()

	events, result, err := collectSessionEvents(t, session)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.LastMessage, "E2E_OK") || !strings.Contains(result.LastMessage, filename) {
		t.Fatalf("final message = %q, want E2E_OK and filename", result.LastMessage)
	}

	data, err := os.ReadFile(filepath.Join(workdir, filename))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if got := string(data); got != expected {
		t.Fatalf("file contents = %q, want %q", got, expected)
	}

	sawTooling := false
	for _, evt := range events {
		if evt.Kind == launcherevents.KindCommandExec || evt.Kind == launcherevents.KindFileChange {
			sawTooling = true
			break
		}
	}
	if !sawTooling {
		t.Fatalf("expected command.exec or file.change event, got %s", eventKinds(events))
	}
}

func TestClaudeStreamSessionRun_EmitsPermissionEvent(t *testing.T) {
	restore := installFakeClaude(t, []string{
		`{"type":"system.init","session_id":"claude-session-1"}`,
		`{"type":"assistant.content_block_delta","delta":{"type":"text_delta","text":"done"}}`,
		`{"type":"assistant.message_stop","usage":{"input_tokens":2,"output_tokens":2,"cache_read_input_tokens":0}}`,
	})
	defer restore()
	t.Setenv("GH_TOKEN", "host-gh-token")

	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "claude-agent",
		Role:    "dev",
		Runtime: config.RuntimeClaudeCode,
		Prompt:  "Run check",
		Policy:  config.PolicyConfig{Sandbox: "read-only"},
		Timeout: 10 * time.Second,
	}
	session, err := newTestLauncher(t).Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = session.Close() }()

	events, result, err := collectSessionEvents(t, session)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	payload := firstPermissionPayload(t, events)
	if payload.Agent != "claude-agent" || payload.Role != "dev" {
		t.Fatalf("agent/role payload = %+v", payload)
	}
	if payload.GitHub.Token != "GH_TOKEN" {
		t.Fatalf("permission token key = %q, want GH_TOKEN", payload.GitHub.Token)
	}
}

func firstPermissionPayload(t *testing.T, events []launcherevents.Event) launcherevents.PermissionPayload {
	t.Helper()
	for _, evt := range events {
		if evt.Kind != launcherevents.KindPermission {
			continue
		}
		var payload launcherevents.PermissionPayload
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			t.Fatalf("unmarshal permission payload: %v", err)
		}
		return payload
	}
	t.Fatalf("missing permission event in %v", eventKinds(events))
	t.Fatal("unreachable")
	return launcherevents.PermissionPayload{}
}

func envHas(env []string, key string) bool {
	_, ok := envGet(env, key)
	return ok
}

func splitNonEmptyLines(data string) []string {
	raw := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func envGet(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

func eventKinds(events []launcherevents.Event) string {
	kinds := make([]string, 0, len(events))
	for _, evt := range events {
		kinds = append(kinds, string(evt.Kind))
	}
	return strings.Join(kinds, ",")
}
