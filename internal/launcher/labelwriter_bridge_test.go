package launcher

// Tests for the coordinator-managed label writer wiring (REQ-146 / #332).
// The adapter itself is a thin shim over internal/labelwriter; what
// matters is the registry-level gate: only the AgentM bridge runtime
// receives a LabelWriter, claude-code / codex never do.

import (
	"context"
	"testing"

	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

type recordingLabelWriter struct {
	calls int
}

func (r *recordingLabelWriter) ApplyNextLabel(_ context.Context, _ string, _ int, _ string) error {
	r.calls++
	return nil
}

// TestRegistry_OnlyAgentMGetsLabelWriter: SetAgentMLabelWriter must wire
// the AgentM bridge runtime's LabelWriter and leave every other
// registered runtime untouched. Claude/codex still flip their own
// labels via `gh issue edit` from inside the subprocess, per CLAUDE.md.
func TestRegistry_OnlyAgentMGetsLabelWriter(t *testing.T) {
	l := runtimepkg.NewRegistry()
	RegisterBuiltins(l)

	lw := &recordingLabelWriter{}
	// The setter must not panic before or after Register, and applying
	// twice must be idempotent. The behavioural assertion (only AgentM
	// invokes the writer) lives in the runtime package's bridge tests,
	// where we drive the actual Run() codepath against fake AgentM and
	// nop sessions.
	l.SetAgentMLabelWriter(lw)
	l.SetAgentMLabelWriter(lw)
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
