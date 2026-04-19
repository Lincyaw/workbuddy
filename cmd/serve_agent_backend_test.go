package cmd

import (
	"os"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
)

func TestLauncherCodexBackendFlagDefault(t *testing.T) {
	// Without WORKBUDDY_CODEX_BACKEND env var, NewLauncher should register
	// the codex-exec runtime (not the mcp-server bridge).
	os.Unsetenv("WORKBUDDY_CODEX_BACKEND")
	l := launcher.NewLauncher()

	// Verify the launcher was created successfully.
	if l == nil {
		t.Fatal("NewLauncher() returned nil")
	}

	// Verify codex runtime is registered by attempting to start with a codex config.
	// We can't fully start (no real subprocess), but we can verify registration
	// by checking that Start doesn't return "unsupported runtime".
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

	// Start will fail because no real codex binary, but the error should NOT be
	// "unsupported runtime" -- it should be a subprocess-related error.
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

func TestLauncherCodexBackendFlagMCPServer(t *testing.T) {
	// With WORKBUDDY_CODEX_BACKEND=mcp-server, NewLauncher should try to
	// register the mcp-server bridge (or fall back to exec if codex unavailable).
	t.Setenv("WORKBUDDY_CODEX_BACKEND", "mcp-server")
	l := launcher.NewLauncher()

	if l == nil {
		t.Fatal("NewLauncher() returned nil")
	}

	// Verify codex runtime is still registered (either bridge or fallback).
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
	errMsg := err.Error()
	if contains(errMsg, "unsupported runtime") {
		t.Fatalf("codex runtime not registered with mcp-server flag: %v", err)
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
