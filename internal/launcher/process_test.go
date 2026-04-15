package launcher

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
)

func TestProcessSessionBuildCommand_UsesPromptForClaude(t *testing.T) {
	task := newTestTask(t)
	sess := &processSession{
		runtimeName: config.RuntimeClaudeCode,
		agent: &config.AgentConfig{
			Runtime: config.RuntimeClaudeCode,
			Prompt:  "review issue {{.Issue.Number}}",
			Policy: config.PolicyConfig{
				Sandbox:  "danger-full-access",
				Approval: "never",
			},
		},
		task: task,
	}

	cmd, err := sess.buildCommand(context.Background())
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if got := cmd.Args[0]; got != "claude" && !strings.HasSuffix(got, "/claude") {
		t.Fatalf("command path = %q", got)
	}
	joined := strings.Join(cmd.Args[1:], " ")
	for _, want := range []string{"--dangerously-skip-permissions", "--output-format stream-json", "--verbose"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
	data, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if got := string(data); got != "review issue 42" {
		t.Fatalf("stdin = %q", got)
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
