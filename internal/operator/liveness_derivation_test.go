package operator

import (
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/liveness"
)

// TestAlertThresholdsDeriveFromLiveness verifies the operator's alert
// thresholds come from the shared liveness source of truth (ADR §6) rather
// than independent local constants that could drift.
func TestAlertThresholdsDeriveFromLiveness(t *testing.T) {
	dl := liveness.Default()
	if leaseExpiredGrace != dl.LeaseExpiredGrace {
		t.Errorf("leaseExpiredGrace = %s, want shared %s", leaseExpiredGrace, dl.LeaseExpiredGrace)
	}
	if taskStuckAfter != dl.PendingTaskStuckAfter {
		t.Errorf("taskStuckAfter = %s, want shared %s", taskStuckAfter, dl.PendingTaskStuckAfter)
	}
	if missingLabelAfter != dl.MissingLabelAfter {
		t.Errorf("missingLabelAfter = %s, want shared %s", missingLabelAfter, dl.MissingLabelAfter)
	}
}

// TestDefaultCheckIntervalDerivesFromLiveness verifies the detector's
// default scan interval falls back to the shared alert-check interval.
func TestDefaultCheckIntervalDerivesFromLiveness(t *testing.T) {
	d := NewDetector(DetectorOptions{})
	if d.cfg.CheckInterval != liveness.Default().AlertCheckInterval {
		t.Errorf("default CheckInterval = %s, want shared %s", d.cfg.CheckInterval, liveness.Default().AlertCheckInterval)
	}
}

// TestWorkerHeartbeatStaleWindow pins the heartbeat staleness window to the
// shared 3× factor (default 15s interval → 45s).
func TestWorkerHeartbeatStaleWindow(t *testing.T) {
	if got := liveness.Default().WorkerHeartbeatStaleAfter(15 * time.Second); got != 45*time.Second {
		t.Errorf("worker heartbeat stale window = %s, want 45s", got)
	}
}
