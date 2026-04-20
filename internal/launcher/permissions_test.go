package launcher

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

	session := newProcessSession(config.RuntimeClaudeCode, agent, task, nil)
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
	session, err := NewLauncher().Start(context.Background(), agent, task)
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
	session, err := NewLauncher().Start(context.Background(), agent, task)
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
