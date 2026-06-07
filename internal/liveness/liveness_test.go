package liveness

import (
	"testing"
	"time"
)

// TestDefaultValidates is the guardrail: the shipped defaults must satisfy
// the layering ordering invariant. A future edit that changes one duration
// in isolation and breaks the ordering fails here.
func TestDefaultValidates(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default() must satisfy the ordering invariant: %v", err)
	}
}

// TestOrderingInvariant documents and enforces the meaningful layering:
// idle_kill <= agent_timeout, and orphan_reaper_grace <= forensic_orphaned_after.
// (idle_kill vs orphan_reaper_grace is intentionally NOT ordered — different
// clocks; see the package doc.)
func TestOrderingInvariant(t *testing.T) {
	d := Default()
	if d.IdleKill > d.AgentTimeout {
		t.Fatalf("idle_kill (%s) must be <= agent_timeout (%s): the watchdog must fire within the agent's own budget",
			d.IdleKill, d.AgentTimeout)
	}
	forensic := d.ForensicOrphanedAfterFor(d.AgentTimeout)
	if d.OrphanReaperGrace > forensic {
		t.Fatalf("orphan_reaper_grace (%s) must be <= forensic_orphaned_after (%s): forensic only flags genuinely-dead tasks",
			d.OrphanReaperGrace, forensic)
	}
}

// TestValidateRejectsBrokenOrdering proves Validate is a real gate: an edit
// that pushes idle_kill past the agent timeout, or shrinks the forensic
// window below the reaper grace, must be rejected.
func TestValidateRejectsBrokenOrdering(t *testing.T) {
	d := Default()
	d.IdleKill = d.AgentTimeout + time.Minute
	if err := d.Validate(); err == nil {
		t.Fatal("Validate must reject idle_kill > agent_timeout")
	}

	d = Default()
	// Force factor 1 and a tiny agent timeout so forensic_orphaned_after
	// (== AgentTimeout) falls below the reaper grace.
	d.ForensicOrphanFactor = 0
	d.AgentTimeout = time.Minute
	if err := d.Validate(); err == nil {
		t.Fatal("Validate must reject orphan_reaper_grace > forensic_orphaned_after")
	}
}

// TestHistoricalEffectiveTimings pins the defaults to the pre-unification
// values of all four detectors. If any of these change, production timing
// changed — which this ADR §6 task explicitly forbids.
func TestHistoricalEffectiveTimings(t *testing.T) {
	d := Default()
	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		// staleinference
		{"idle_kill", d.IdleKill, 10 * time.Minute},
		{"idle_check_interval", d.IdleCheckInterval, 30 * time.Second},
		{"completed_grace_period", d.CompletedGracePeriod, time.Minute},
		// taskreaper
		{"orphan_reaper_grace", d.OrphanReaperGrace, 5 * time.Minute},
		{"reaper_interval", d.ReaperInterval, 60 * time.Second},
		// operator
		{"alert_check_interval", d.AlertCheckInterval, 60 * time.Second},
		{"lease_expired_grace", d.LeaseExpiredGrace, 30 * time.Second},
		{"pending_task_stuck_after", d.PendingTaskStuckAfter, 10 * time.Minute},
		{"missing_label_after", d.MissingLabelAfter, 5 * time.Minute},
		// diagnose
		{"agent_timeout", d.AgentTimeout, 60 * time.Minute},
		{"forensic_orphaned_after", d.ForensicOrphanedAfterFor(d.AgentTimeout), 120 * time.Minute},
		{"stuck_issue_threshold", d.StuckIssueThreshold, time.Hour},
		{"no_child_grace_period", d.NoChildGracePeriod, 2 * time.Minute},
		{"worker_heartbeat_stale", d.WorkerHeartbeatStaleAfter(15 * time.Second), 45 * time.Second},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: effective timing changed: got %s, want %s (production timing must not change)", c.name, c.got, c.want)
		}
	}
}

func TestForensicOrphanedAfterForFallback(t *testing.T) {
	d := Default()
	if got := d.ForensicOrphanedAfterFor(0); got != 120*time.Minute {
		t.Fatalf("zero timeout must fall back to 2×AgentTimeout: got %s", got)
	}
	if got := d.ForensicOrphanedAfterFor(30 * time.Minute); got != 60*time.Minute {
		t.Fatalf("explicit 30m timeout must give 60m: got %s", got)
	}
}
