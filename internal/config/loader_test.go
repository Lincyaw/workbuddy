package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_RejectsUnknownWorkflowStateKeys(t *testing.T) {
	t.Helper()

	configDir := t.TempDir()
	workflowsDir := filepath.Join(configDir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}

	workflow := "---\n" +
		"name: default\n" +
		"description: test workflow\n" +
		"trigger:\n" +
		"  issue_label: \"workbuddy\"\n" +
		"max_retries: 3\n" +
		"---\n\n" +
		"## Test Workflow\n\n" +
		"Unknown workflow state keys should fail loudly so rollout config typos do\n" +
		"not slip through validation.\n\n" +
		"```yaml\n" +
		"states:\n" +
		"  reviewing:\n" +
		"    enter_label: \"status:reviewing\"\n" +
		"    agent: review-agent\n" +
		"    transitions:\n" +
		"      \"status:done\": done\n" +
		"  done:\n" +
		"    enter_label: \"status:done\"\n" +
		"    action: close_issue\n" +
		"```\n"
	path := filepath.Join(workflowsDir, "default.md")
	if err := os.WriteFile(path, []byte(workflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	cfg, warnings, err := LoadConfig(configDir)
	if err == nil {
		t.Fatal("expected unknown workflow state key to fail")
	}
	if cfg != nil || len(warnings) != 0 {
		t.Fatalf("expected failed load with no partial config/warnings, got cfg=%v warnings=%v", cfg != nil, warnings)
	}
}

func TestLoadConfig_ParsesRolloutStateConfig(t *testing.T) {
	t.Helper()

	configDir := t.TempDir()
	workflowsDir := filepath.Join(configDir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}

	workflow := "---\n" +
		"name: default\n" +
		"description: rollout workflow\n" +
		"trigger:\n" +
		"  issue_label: \"workbuddy\"\n" +
		"max_retries: 3\n" +
		"---\n\n" +
		"```yaml\n" +
		"states:\n" +
		"  developing:\n" +
		"    enter_label: \"status:developing\"\n" +
		"    agent: dev-agent\n" +
		"    rollouts: 3\n" +
		"    join:\n" +
		"      strategy: rollouts\n" +
		"      min_successes: 2\n" +
		"    transitions:\n" +
		"      \"status:reviewing\": reviewing\n" +
		"  reviewing:\n" +
		"    enter_label: \"status:reviewing\"\n" +
		"```\n"
	path := filepath.Join(workflowsDir, "default.md")
	if err := os.WriteFile(path, []byte(workflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	cfg, _, err := LoadConfig(configDir)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	state := cfg.Workflows["default"].States["developing"]
	if state.Rollouts != 3 {
		t.Fatalf("rollouts = %d, want 3", state.Rollouts)
	}
	if state.Join.Strategy != JoinRollouts {
		t.Fatalf("join.strategy = %q, want %q", state.Join.Strategy, JoinRollouts)
	}
	if state.Join.MinSuccesses != 2 {
		t.Fatalf("join.min_successes = %d, want 2", state.Join.MinSuccesses)
	}
}

func TestLoadConfig_RejectsInvalidRolloutJoin(t *testing.T) {
	t.Helper()

	configDir := t.TempDir()
	workflowsDir := filepath.Join(configDir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}

	workflow := "---\n" +
		"name: default\n" +
		"description: rollout workflow\n" +
		"trigger:\n" +
		"  issue_label: \"workbuddy\"\n" +
		"max_retries: 3\n" +
		"---\n\n" +
		"```yaml\n" +
		"states:\n" +
		"  developing:\n" +
		"    enter_label: \"status:developing\"\n" +
		"    agent: dev-agent\n" +
		"    rollouts: 3\n" +
		"    join:\n" +
		"      strategy: all_passed\n" +
		"    transitions:\n" +
		"      \"status:reviewing\": reviewing\n" +
		"  reviewing:\n" +
		"    enter_label: \"status:reviewing\"\n" +
		"```\n"
	path := filepath.Join(workflowsDir, "default.md")
	if err := os.WriteFile(path, []byte(workflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	if _, _, err := LoadConfig(configDir); err == nil {
		t.Fatal("expected rollout join config error")
	}
}

func TestLoadConfig_RejectsUnknownRolloutJoinKey(t *testing.T) {
	t.Helper()

	configDir := t.TempDir()
	workflowsDir := filepath.Join(configDir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}

	workflow := "---\n" +
		"name: default\n" +
		"description: rollout workflow\n" +
		"trigger:\n" +
		"  issue_label: \"workbuddy\"\n" +
		"max_retries: 3\n" +
		"---\n\n" +
		"```yaml\n" +
		"states:\n" +
		"  developing:\n" +
		"    enter_label: \"status:developing\"\n" +
		"    agent: dev-agent\n" +
		"    rollouts: 3\n" +
		"    join:\n" +
		"      strategy: rollouts\n" +
		"      min_successes: 2\n" +
		"      typo: true\n" +
		"    transitions:\n" +
		"      \"status:reviewing\": reviewing\n" +
		"  reviewing:\n" +
		"    enter_label: \"status:reviewing\"\n" +
		"```\n"
	path := filepath.Join(workflowsDir, "default.md")
	if err := os.WriteFile(path, []byte(workflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	if _, _, err := LoadConfig(configDir); err == nil {
		t.Fatal("expected unknown rollout join key to fail")
	}
}

// TestNormalizeAgentConfig_AcceptsAgentMRuntime covers AC-1-1 of issue #315:
// `runtime: agentm` must pass config normalization with the same sandbox /
// approval set as `codex`. v0.5 is host-exec only — see
// docs/planned/agentm-runtime.md and docs/decisions/2026-05-13-k8s-agentm-otel.md.
func TestNormalizeAgentConfig_AcceptsAgentMRuntime(t *testing.T) {
	t.Helper()

	cases := []struct {
		name    string
		policy  PolicyConfig
		wantSbx string
		wantApp string
	}{
		{
			name:    "defaults_fill_in",
			policy:  PolicyConfig{},
			wantSbx: "read-only",
			wantApp: "never",
		},
		{
			name:    "workspace_write_ok",
			policy:  PolicyConfig{Sandbox: "workspace-write", Approval: "on-failure"},
			wantSbx: "workspace-write",
			wantApp: "on-failure",
		},
		{
			name:    "danger_full_access_ok",
			policy:  PolicyConfig{Sandbox: "danger-full-access", Approval: "via-approver"},
			wantSbx: "danger-full-access",
			wantApp: "via-approver",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &AgentConfig{
				Name:    "agentm-dev",
				Runtime: RuntimeAgentM,
				Policy:  tc.policy,
			}
			warnings, err := NormalizeAgentConfig(agent)
			if err != nil {
				t.Fatalf("NormalizeAgentConfig: unexpected error: %v (warnings=%v)", err, warnings)
			}
			if agent.Runtime != RuntimeAgentM {
				t.Errorf("Runtime was rewritten to %q; agentm must stay agentm", agent.Runtime)
			}
			if agent.Policy.Sandbox != tc.wantSbx {
				t.Errorf("Policy.Sandbox = %q, want %q", agent.Policy.Sandbox, tc.wantSbx)
			}
			if agent.Policy.Approval != tc.wantApp {
				t.Errorf("Policy.Approval = %q, want %q", agent.Policy.Approval, tc.wantApp)
			}
		})
	}
}

// TestNormalizeAgentConfig_DevContainerImage covers REQ-140 / issue #328
// AC-1-1 + AC-1-2: dev_container_image validates cleanly with runtime=agentm
// (no warning, no error); setting it on runtime=claude-code produces a
// warning (not an error) so configs can stage the field for an upcoming
// runtime swap.
func TestNormalizeAgentConfig_DevContainerImage(t *testing.T) {
	t.Helper()

	t.Run("agentm_no_warning", func(t *testing.T) {
		agent := &AgentConfig{
			Name:              "agentm-dev",
			Runtime:           RuntimeAgentM,
			DevContainerImage: "ghcr.io/lincyaw/workbuddy-dev:latest",
		}
		warnings, err := NormalizeAgentConfig(agent)
		if err != nil {
			t.Fatalf("NormalizeAgentConfig: %v", err)
		}
		for _, w := range warnings {
			if strings.Contains(w.Message, "dev_container_image") {
				t.Fatalf("unexpected dev_container_image warning for agentm runtime: %s", w.Message)
			}
		}
		if agent.DevContainerImage != "ghcr.io/lincyaw/workbuddy-dev:latest" {
			t.Fatalf("DevContainerImage should be preserved, got %q", agent.DevContainerImage)
		}
	})

	t.Run("claude_code_warns_not_errors", func(t *testing.T) {
		agent := &AgentConfig{
			Name:              "claude-dev",
			Runtime:           RuntimeClaudeCode,
			Policy:            PolicyConfig{Sandbox: "read-only", Approval: "never"},
			DevContainerImage: "ghcr.io/x:y",
		}
		warnings, err := NormalizeAgentConfig(agent)
		if err != nil {
			t.Fatalf("NormalizeAgentConfig: expected nil error, got %v", err)
		}
		found := false
		for _, w := range warnings {
			if strings.Contains(w.Message, "dev_container_image") && strings.Contains(w.Message, "claude-code") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected dev_container_image warning for runtime=claude-code, got warnings=%v", warnings)
		}
	})

	t.Run("agentm_empty_no_warning", func(t *testing.T) {
		agent := &AgentConfig{
			Name:    "agentm-dev",
			Runtime: RuntimeAgentM,
		}
		warnings, err := NormalizeAgentConfig(agent)
		if err != nil {
			t.Fatalf("NormalizeAgentConfig: %v", err)
		}
		for _, w := range warnings {
			if strings.Contains(w.Message, "dev_container_image") {
				t.Fatalf("unexpected warning when field unset: %s", w.Message)
			}
		}
	})
}

// TestNormalizeAgentConfig_RejectsBadAgentMPolicy verifies the policy matrix
// rejects clearly-invalid combinations, mirroring the codex matrix.
func TestNormalizeAgentConfig_RejectsBadAgentMPolicy(t *testing.T) {
	t.Helper()

	cases := []PolicyConfig{
		{Sandbox: "no-such-sandbox", Approval: "never"},
		{Sandbox: "read-only", Approval: "no-such-approval"},
	}
	for _, p := range cases {
		agent := &AgentConfig{
			Name:    "agentm-dev",
			Runtime: RuntimeAgentM,
			Policy:  p,
		}
		if _, err := NormalizeAgentConfig(agent); err == nil {
			t.Errorf("policy %+v: expected error, got nil", p)
		}
	}
}

// TestLoadConfig_AcceptsAgentMAgentFile covers AC-1-1 end-to-end: an agent
// frontmatter declaring `runtime: agentm` loads cleanly from disk via
// LoadConfig.
func TestLoadConfig_AcceptsAgentMAgentFile(t *testing.T) {
	t.Helper()

	configDir := t.TempDir()
	agentsDir := filepath.Join(configDir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}

	agentMD := "---\n" +
		"name: agentm-dev\n" +
		"description: AgentM-powered dev agent (v0.5 contract only)\n" +
		"role: dev\n" +
		"runtime: agentm\n" +
		"triggers:\n" +
		"  - state: developing\n" +
		"context:\n" +
		"  - Repo\n" +
		"  - Issue.Number\n" +
		"policy:\n" +
		"  sandbox: workspace-write\n" +
		"  approval: never\n" +
		"  timeout: 30m\n" +
		"---\n\n" +
		"Implement issue {{.Issue.Number}} in {{.Repo}}.\n"
	path := filepath.Join(agentsDir, "agentm-dev.md")
	if err := os.WriteFile(path, []byte(agentMD), 0o644); err != nil {
		t.Fatalf("write agent: %v", err)
	}

	cfg, _, err := LoadConfig(configDir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	agent, ok := cfg.Agents["agentm-dev"]
	if !ok {
		t.Fatalf("agent agentm-dev not loaded; got %+v", cfg.Agents)
	}
	if agent.Runtime != RuntimeAgentM {
		t.Errorf("Runtime = %q, want %q", agent.Runtime, RuntimeAgentM)
	}
	if agent.Policy.Sandbox != "workspace-write" {
		t.Errorf("Policy.Sandbox = %q, want workspace-write", agent.Policy.Sandbox)
	}
}
