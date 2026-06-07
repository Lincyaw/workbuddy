package diagnose

import (
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/liveness"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestForensicThresholdsDeriveFromLiveness verifies diagnose's forensic
// thresholds come from the shared liveness source of truth (ADR §6).
func TestForensicThresholdsDeriveFromLiveness(t *testing.T) {
	dl := liveness.Default()
	if stuckThreshold != dl.StuckIssueThreshold {
		t.Errorf("stuckThreshold = %s, want shared %s", stuckThreshold, dl.StuckIssueThreshold)
	}
	if defaultAgentTimeout != dl.AgentTimeout {
		t.Errorf("defaultAgentTimeout = %s, want shared %s", defaultAgentTimeout, dl.AgentTimeout)
	}
	if defaultIdleThreshold != dl.IdleKill {
		t.Errorf("defaultIdleThreshold = %s, want shared %s", defaultIdleThreshold, dl.IdleKill)
	}
	if noChildGracePeriod != dl.NoChildGracePeriod {
		t.Errorf("noChildGracePeriod = %s, want shared %s", noChildGracePeriod, dl.NoChildGracePeriod)
	}
	if TunnelHeartbeatStaleAfter != dl.WorkerHeartbeatStaleAfter(15*time.Second) {
		t.Errorf("TunnelHeartbeatStaleAfter = %s, want shared %s", TunnelHeartbeatStaleAfter, dl.WorkerHeartbeatStaleAfter(15*time.Second))
	}
}

// TestOrphanedThresholdDerivesFromLiveness verifies the per-task forensic
// orphaned-after uses the shared 2×timeout derivation, and that the result
// stays ordered after the reaper grace (the layering invariant).
func TestOrphanedThresholdDerivesFromLiveness(t *testing.T) {
	dl := liveness.Default()

	// No explicit agent timeout → falls back to 2 × default agent timeout.
	got := orphanedThresholdForTask(store.TaskRecord{AgentName: "dev"}, map[string]time.Duration{})
	want := dl.ForensicOrphanedAfterFor(dl.AgentTimeout)
	if got != want {
		t.Errorf("orphanedThresholdForTask fallback = %s, want %s", got, want)
	}
	if got < dl.OrphanReaperGrace {
		t.Errorf("forensic orphaned-after (%s) must be >= reaper grace (%s)", got, dl.OrphanReaperGrace)
	}

	// Explicit agent timeout flows through the shared derivation.
	got = orphanedThresholdForTask(store.TaskRecord{AgentName: "dev"}, map[string]time.Duration{"dev": 30 * time.Minute})
	if want := dl.ForensicOrphanedAfterFor(30 * time.Minute); got != want {
		t.Errorf("orphanedThresholdForTask(30m) = %s, want %s", got, want)
	}
}
