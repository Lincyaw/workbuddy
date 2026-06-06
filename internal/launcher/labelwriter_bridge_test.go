package launcher

// Tests for the coordinator-managed label writer wiring (REQ-146 / #332).
// The adapter itself is a thin shim over internal/labelwriter; what
// matters is the registry-level gate: only the AgentM bridge runtime
// receives a LabelWriter, claude-code / codex never do.

import (
	"context"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

type recordingLabelWriter struct {
	calls int
}

func (r *recordingLabelWriter) ApplyNextLabel(_ context.Context, _ string, _ int, _ string) error {
	r.calls++
	return nil
}

// TestRegistry_OnlyAgentMGetsLabelWriter: the capability-driven
// SetLabelWriter must wire the LabelWriter onto runtimes whose
// Capabilities().ManagesOwnLabels is false (agentm) and leave every
// self-managed runtime (claude/codex) untouched — they keep flipping their
// own labels via `gh issue edit` from inside the subprocess (ADR §3).
func TestRegistry_OnlyAgentMGetsLabelWriter(t *testing.T) {
	l := runtimepkg.NewRegistry()
	RegisterBuiltins(l)

	lw := &recordingLabelWriter{}
	// The setter must not panic before or after Register, and applying
	// twice must be idempotent. The behavioural assertion (only AgentM
	// invokes the writer) lives in the runtime package's bridge tests,
	// where we drive the actual Run() codepath against fake AgentM and
	// nop sessions.
	l.SetLabelWriter(lw)
	l.SetLabelWriter(lw)

	// The agentm runtime (ManagesOwnLabels=false) must receive the writer;
	// the self-managed runtimes must not.
	if got := agentMRuntimeLabelWriter(l); got != lw {
		t.Fatalf("agentm runtime did not receive the capability-wired label writer (got %v)", got)
	}
	for _, name := range []string{config.RuntimeClaudeCode, config.RuntimeCodex} {
		if rt := l.RuntimeByName(name); rt != nil {
			if !rt.Capabilities().ManagesOwnLabels {
				t.Fatalf("self-managed runtime %q must report ManagesOwnLabels=true", name)
			}
		}
	}
}

// agentMRuntimeLabelWriter returns the LabelWriter wired onto the registered
// agentm runtime, or nil if none/not an *AgentMRuntime.
func agentMRuntimeLabelWriter(l *runtimepkg.Registry) runtimepkg.AgentMLabelWriter {
	if rt, ok := l.RuntimeByName(config.RuntimeAgentM).(*runtimepkg.AgentMRuntime); ok {
		return rt.LabelWriter
	}
	return nil
}

// TestNewAgentMLabelWriterAdapter_NilStoreIsNoOp: the adapter must
// tolerate a nil store (test scaffolding paths) by treating ApplyNextLabel
// as a no-op, so unit tests that construct a Launcher without a store
// don't crash on a stray label write.
func TestNewAgentMLabelWriterAdapter_NilStoreIsNoOp(t *testing.T) {
	a := NewAgentMLabelWriterAdapter(nil)
	if err := a.ApplyNextLabel(context.Background(), "r/r", 1, "status:reviewing"); err != nil {
		t.Fatalf("expected nil-store adapter to be a no-op, got err=%v", err)
	}
}
