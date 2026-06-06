package worker

import (
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/workerclient"
)

// TestResolveAgentConfigPrefersWireConfig verifies the worker uses the
// per-repo agent config shipped over the wire (ADR 2026-06-06 §2) in
// preference to its own local config, and falls back to local config only
// when the wire field is nil.
func TestResolveAgentConfigPrefersWireConfig(t *testing.T) {
	localCfg := &config.FullConfig{
		Agents: map[string]*config.AgentConfig{
			"dev": {Name: "dev", Runtime: "claude-code", DevContainerImage: "local/dev:1"},
		},
	}
	w := NewDistributedWorker(DistributedDeps{Config: localCfg})

	wireAgent := &config.AgentConfig{Name: "dev", Runtime: "agentm", DevContainerImage: "repo-b/dev:2"}
	got, err := w.resolveAgentConfig(&workerclient.Task{AgentName: "dev", Agent: wireAgent})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wireAgent {
		t.Fatalf("expected wire-supplied agent config, got %+v", got)
	}
	if got.DevContainerImage != "repo-b/dev:2" {
		t.Fatalf("expected per-repo image from wire, got %q", got.DevContainerImage)
	}
}

func TestResolveAgentConfigFallsBackToLocal(t *testing.T) {
	localCfg := &config.FullConfig{
		Agents: map[string]*config.AgentConfig{
			"dev": {Name: "dev", Runtime: "claude-code", DevContainerImage: "local/dev:1"},
		},
	}
	w := NewDistributedWorker(DistributedDeps{Config: localCfg})

	got, err := w.resolveAgentConfig(&workerclient.Task{AgentName: "dev", Agent: nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != localCfg.Agents["dev"] {
		t.Fatalf("expected local fallback agent config, got %+v", got)
	}
}

func TestResolveAgentConfigMissingLocalErrors(t *testing.T) {
	w := NewDistributedWorker(DistributedDeps{Config: &config.FullConfig{
		Agents: map[string]*config.AgentConfig{},
	}})

	if _, err := w.resolveAgentConfig(&workerclient.Task{AgentName: "ghost", Agent: nil}); err == nil {
		t.Fatal("expected error for missing local agent, got nil")
	}
}
