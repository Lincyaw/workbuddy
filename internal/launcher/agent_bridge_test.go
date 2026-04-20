package launcher

import (
	"context"
	"os"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
)

func TestNewBackendFromConfigClaudeCode(t *testing.T) {
	b, err := newBackendFromConfig(config.RuntimeClaudeCode)
	if err != nil {
		t.Fatalf("newBackendFromConfig(%q) error: %v", config.RuntimeClaudeCode, err)
	}
	if b == nil {
		t.Fatalf("newBackendFromConfig(%q) = nil, want non-nil claude backend", config.RuntimeClaudeCode)
	}
}

func TestNewBackendFromConfigClaudeShot(t *testing.T) {
	b, err := newBackendFromConfig(config.RuntimeClaudeShot)
	if err != nil {
		t.Fatalf("newBackendFromConfig(%q) error: %v", config.RuntimeClaudeShot, err)
	}
	if b == nil {
		t.Fatalf("newBackendFromConfig(%q) = nil, want non-nil claude backend", config.RuntimeClaudeShot)
	}
}

func TestNewBackendFromConfigCodexExecNoEnv(t *testing.T) {
	// Without WORKBUDDY_CODEX_BACKEND env var, should return nil (fall through).
	os.Unsetenv("WORKBUDDY_CODEX_BACKEND")
	b, err := newBackendFromConfig(config.RuntimeCodexExec)
	if err != nil {
		t.Fatalf("newBackendFromConfig(%q) error: %v", config.RuntimeCodexExec, err)
	}
	if b != nil {
		t.Fatalf("newBackendFromConfig(%q) = %v, want nil (fall through)", config.RuntimeCodexExec, b)
	}
}

func TestNewBackendFromConfigCodexNoEnv(t *testing.T) {
	os.Unsetenv("WORKBUDDY_CODEX_BACKEND")
	b, err := newBackendFromConfig(config.RuntimeCodex)
	if err != nil {
		t.Fatalf("newBackendFromConfig(%q) error: %v", config.RuntimeCodex, err)
	}
	if b != nil {
		t.Fatalf("newBackendFromConfig(%q) = %v, want nil (fall through)", config.RuntimeCodex, b)
	}
}

func TestNewBackendFromConfigCodexAppServerRuntime(t *testing.T) {
	b, err := newBackendFromConfig(config.RuntimeCodexServer)
	if err != nil {
		t.Logf("newBackendFromConfig(%q): %v (expected when codex is not installed)", config.RuntimeCodexServer, err)
		return
	}
	if b == nil {
		t.Fatal("newBackendFromConfig(codex-appserver) returned nil backend")
	}
}

func TestNewBackendFromConfigCodexAppServerEnv(t *testing.T) {
	// With WORKBUDDY_CODEX_BACKEND=app-server, should attempt to create codex backend.
	t.Setenv("WORKBUDDY_CODEX_BACKEND", "app-server")
	b, err := newBackendFromConfig(config.RuntimeCodex)
	if err != nil {
		t.Logf("newBackendFromConfig with app-server env: %v (expected when codex is not installed)", err)
		return
	}
	if b == nil {
		t.Fatal("newBackendFromConfig with app-server env returned nil backend")
	}
	_ = b.Shutdown(context.Background())
}

func TestNewBackendFromConfigUnknown(t *testing.T) {
	_, err := newBackendFromConfig("unknown-runtime")
	if err == nil {
		t.Fatal("newBackendFromConfig(unknown) should return error")
	}
}

func TestAgentBridgeRuntimeName(t *testing.T) {
	r := &agentBridgeRuntime{runtimeName: "test-runtime"}
	if r.Name() != "test-runtime" {
		t.Fatalf("Name() = %q, want %q", r.Name(), "test-runtime")
	}
}

func TestAgentBridgeSessionSetApprover(t *testing.T) {
	s := &agentBridgeSession{}
	err := s.SetApprover(AlwaysAllow{})
	if err != ErrNotSupported {
		t.Fatalf("SetApprover() = %v, want ErrNotSupported", err)
	}
}
