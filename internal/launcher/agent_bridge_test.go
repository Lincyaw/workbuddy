package launcher

import (
	"context"
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

func TestNewBackendFromConfigCodex(t *testing.T) {
	restore := installFakeCodex(t)
	defer restore()

	b, err := newBackendFromConfig(config.RuntimeCodex)
	if err != nil {
		t.Fatalf("newBackendFromConfig(%q) error: %v", config.RuntimeCodex, err)
	}
	if b == nil {
		t.Fatalf("newBackendFromConfig(%q) = nil, want codex backend", config.RuntimeCodex)
	}
	_ = b.Shutdown(context.Background())
}

func TestNewBackendFromConfigCodexAppServerRuntime(t *testing.T) {
	restore := installFakeCodex(t)
	defer restore()

	b, err := newBackendFromConfig(config.RuntimeCodexServer)
	if err != nil {
		t.Fatalf("newBackendFromConfig(%q): %v", config.RuntimeCodexServer, err)
	}
	if b == nil {
		t.Fatal("newBackendFromConfig(codex-appserver) returned nil backend")
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
	r := newAgentBridgeRuntime("test-runtime", nil)
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
