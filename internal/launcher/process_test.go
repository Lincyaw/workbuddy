package launcher

import (
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

func TestProcessSessionBuildSpec_UsesPromptForClaude(t *testing.T) {
	task := newTestTask(t)
	sess := &runtimepkg.ProcessSession{
		RuntimeName: config.RuntimeClaudeCode,
		Agent: &config.AgentConfig{
			Runtime: config.RuntimeClaudeCode,
			Prompt:  "review issue {{.Issue.Number}}",
			Policy: config.PolicyConfig{
				Sandbox:  "danger-full-access",
				Approval: "never",
			},
		},
		Task: task,
	}

	spec, err := sess.BuildSpec()
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if spec.Binary != "claude" {
		t.Fatalf("binary = %q, want claude", spec.Binary)
	}
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{"--dangerously-skip-permissions", "--output-format stream-json", "--verbose"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
	if spec.Stdin != "review issue 42" {
		t.Fatalf("stdin = %q", spec.Stdin)
	}
}

func TestProcessSessionBuildSpec_AddsRolloutFlagsOnlyForRollouts(t *testing.T) {
	task := newTestTask(t)
	task.Rollout = runtimepkg.RolloutContext{Index: 2, Total: 3, GroupID: "rollout-group"}

	sess := &runtimepkg.ProcessSession{
		RuntimeName: config.RuntimeClaudeCode,
		Agent: &config.AgentConfig{
			Runtime: config.RuntimeClaudeCode,
			Prompt:  "review issue {{.Issue.Number}}",
			Policy: config.PolicyConfig{
				Sandbox:  "read-only",
				Approval: "never",
			},
		},
		Task: task,
	}

	spec, err := sess.BuildSpec()
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{"--rollout-index 2", "--rollouts-total 3"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}

	task.Rollout = runtimepkg.RolloutContext{}
	spec, err = sess.BuildSpec()
	if err != nil {
		t.Fatalf("BuildSpec without rollout: %v", err)
	}
	joined = strings.Join(spec.Args, " ")
	if strings.Contains(joined, "--rollout-index") || strings.Contains(joined, "--rollouts-total") {
		t.Fatalf("non-rollout args should omit rollout flags, got %q", joined)
	}
}

func TestClaudePolicyArgs_SandboxGatesPermissionBypass(t *testing.T) {
	cases := []struct {
		name       string
		sandbox    string
		wantBypass bool
	}{
		{"read-only", "read-only", false},
		{"workspace-write", "workspace-write", false},
		{"danger-full-access", "danger-full-access", true},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := claudePolicyArgs(config.PolicyConfig{Sandbox: tc.sandbox})
			joined := strings.Join(args, " ")
			has := strings.Contains(joined, "--dangerously-skip-permissions")
			if has != tc.wantBypass {
				t.Fatalf("sandbox=%q args=%q bypass=%v want %v", tc.sandbox, joined, has, tc.wantBypass)
			}
		})
	}
}
