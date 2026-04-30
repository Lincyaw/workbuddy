package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_IgnoresUnknownWorkflowStateKeys(t *testing.T) {
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
		"The loader currently ignores unknown keys inside workflow state blocks so\n" +
		"legacy decorative knobs like action do not break config parsing.\n\n" +
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
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings for unknown workflow state keys, got %v", warnings)
	}

	wf := cfg.Workflows["default"]
	if wf == nil {
		t.Fatalf("expected workflow to be loaded")
	}

	done := wf.States["done"]
	if done == nil {
		t.Fatalf("expected done state to be loaded")
	}
	if done.EnterLabel != "status:done" {
		t.Fatalf("unexpected done enter label %q", done.EnterLabel)
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
