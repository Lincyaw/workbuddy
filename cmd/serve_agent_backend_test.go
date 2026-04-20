package cmd

import (
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
)

func TestLauncherCodexBackendAlwaysRegistered(t *testing.T) {
	l := launcher.NewLauncher()

	if l == nil {
		t.Fatal("NewLauncher() returned nil")
	}

	task := &launcher.TaskContext{
		Issue: launcher.IssueContext{
			Number: 1,
			Title:  "test",
		},
		Repo:    "test/repo",
		WorkDir: t.TempDir(),
	}
	agentCfg := &config.AgentConfig{
		Name:    "test-agent",
		Runtime: config.RuntimeCodex,
	}

	_, err := l.Start(t.Context(), agentCfg, task)
	if err == nil {
		t.Log("Start succeeded (codex binary available)")
		return
	}
	// Verify the error is not about unsupported runtime.
	errMsg := err.Error()
	if contains(errMsg, "unsupported runtime") {
		t.Fatalf("codex runtime not registered: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
