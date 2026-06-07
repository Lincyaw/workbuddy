// Package liveness is the single source of truth for the durations used by
// workbuddy's feedback / error-correction layer — the four stuck-detection
// mechanisms that watch a dispatched agent and its task row.
//
// Background (ADR 2026-06-06 §6). Four mechanisms historically hardcoded
// independent threshold constants in four packages, which could silently
// drift into contradiction (the audit found staleinference could kill an
// agent ~110 min before diagnose would even classify it stuck). These
// mechanisms measure DIFFERENT things, so they are depth layers, not
// duplicates:
//
//	staleinference (internal/staleinference) — in-execution watchdog.
//	    Measures idle-since-last-session-output for a *live* agent process
//	    and KILLS it via context cancellation. Acts first, deepest layer.
//
//	taskreaper (internal/app) — orphan reaper.
//	    Measures no-heartbeat age of a status=running task row and MARKS it
//	    failed so the recovery sweep can redispatch. Acts after the
//	    watchdog has had its chance.
//
//	operator (internal/operator) — alerting detector.
//	    Measures lease-expiry / pending-stall / missing-label and EMITS
//	    alerts. Observes, never mutates task state.
//
//	diagnose (internal/diagnose) — on-demand forensic CLI.
//	    Measures total-runtime orphan age and FLAGS genuinely-dead tasks.
//	    Takes no action. Loosest thresholds so it only reports tasks that
//	    every faster layer has already had time to handle.
//
// These layers do NOT all measure the same clock, which is why some
// thresholds are not directly comparable:
//
//   - IdleKill (watchdog) measures *agent* idleness — time since the running
//     agent last wrote session output, on the worker host. It catches a hung
//     agent while its worker is still alive and heart-beating.
//   - OrphanReaperGrace (reaper) measures *worker* liveness — time since the
//     task row last received a heartbeat, on the coordinator. It catches a
//     dead/partitioned worker, not a hung-but-heart-beating agent.
//
// Because IdleKill and OrphanReaperGrace key off different signals on
// different hosts, ordering them against each other is meaningless and is
// deliberately NOT enforced (the historical values are 10m vs 5m). The
// invariants that ARE meaningful — and enforced by Validate — keep each
// detector ordered against the budget it actually shares a clock with:
//
//	IdleKill <= AgentTimeout
//	    the in-execution watchdog must fire well within the agent's own
//	    runtime budget, so a hung agent is killed before (not after) its
//	    policy timeout would have ended it.
//	OrphanReaperGrace <= ForensicOrphanedAfter
//	    the on-demand forensic tool only flags tasks the automatic reaper has
//	    already had time to handle, so a "diagnose" report never contradicts
//	    a reap that is still pending.
//
// The point of single-sourcing these is NOT to retune production timing —
// the defaults below reproduce the historical effective values exactly — but
// to remove drift potential and to make the meaningful relationships an
// enforced, tested invariant (see Defaults.Validate).
package liveness

import (
	"fmt"
	"time"
)

// Defaults holds the canonical liveness durations shared by the four
// detectors. A future edit that changes one value in isolation and breaks
// the layering ordering will fail Validate (and the package test).
//
// Each detector DERIVES its own threshold from this struct rather than
// hardcoding a constant. Config knobs (worker.stale_inference.*) still
// override the staleinference values per the documented precedence; the
// fields here are the fallbacks those knobs default to.
type Defaults struct {
	// IdleKill is the maximum time since last session output before the
	// in-execution watchdog (staleinference) kills a live agent. This is
	// the FIRST and DEEPEST layer: it acts on a process that is still
	// running but producing no output. Matches the historical
	// staleinference IdleThreshold fallback.
	IdleKill time.Duration

	// IdleCheckInterval is how often the in-execution watchdog polls for
	// staleness. Matches the historical staleinference CheckInterval
	// fallback.
	IdleCheckInterval time.Duration

	// CompletedGracePeriod is the shorter idle window applied once an agent
	// has reported completion but not yet exited. Matches the historical
	// staleinference CompletedGracePeriod fallback.
	CompletedGracePeriod time.Duration

	// OrphanReaperGrace is the no-heartbeat age after which the orphan
	// reaper marks a status=running task row failed. It keys off *worker*
	// heartbeat liveness (coordinator clock), not agent idleness, so it is
	// deliberately not ordered against IdleKill (see the package doc).
	// Validate only requires it stay <= ForensicOrphanedAfter. Matches the
	// historical taskreaper DefaultTaskReaperGrace.
	OrphanReaperGrace time.Duration

	// ReaperInterval is how often the orphan reaper sweeps. Matches the
	// historical taskreaper DefaultTaskReaperInterval.
	ReaperInterval time.Duration

	// AlertCheckInterval is how often the alerting detector (operator)
	// scans coordinator state. Matches the historical operator
	// CheckInterval default.
	AlertCheckInterval time.Duration

	// LeaseExpiredGrace is the slack past a task's lease expiry before the
	// alerting detector emits a lease_expired alert. Matches the historical
	// operator leaseExpiredGrace.
	LeaseExpiredGrace time.Duration

	// PendingTaskStuckAfter is how long a pending task with no online
	// worker may sit before the alerting detector flags it. Matches the
	// historical operator taskStuckAfter.
	PendingTaskStuckAfter time.Duration

	// MissingLabelAfter is how long an issue may carry the workbuddy label
	// without a status:* label before the alerting detector flags it.
	// Matches the historical operator missingLabelAfter.
	MissingLabelAfter time.Duration

	// AgentTimeout is the fallback per-agent runtime budget used by the
	// forensic tool when an agent declares no explicit policy.timeout.
	// Matches the historical diagnose defaultAgentTimeout.
	AgentTimeout time.Duration

	// ForensicOrphanFactor is the multiplier applied to a task's effective
	// agent timeout to derive its orphaned-after threshold in the forensic
	// tool. Matches the historical diagnose "2 × timeout".
	ForensicOrphanFactor int

	// StuckIssueThreshold is how long an issue may sit in an intermediate
	// state with no active task and no events before the forensic tool
	// flags it. Matches the historical diagnose stuckThreshold.
	StuckIssueThreshold time.Duration

	// NoChildGracePeriod is the grace after task start before the forensic
	// tool treats a missing child process as a zombie signal. Matches the
	// historical diagnose noChildGracePeriod.
	NoChildGracePeriod time.Duration

	// WorkerHeartbeatStaleFactor multiplies the worker heartbeat interval
	// to derive the staleness window for "worker missing" / "tunnel down"
	// signals. Matches the historical operator/diagnose 3× heartbeat
	// window (default 15s interval → 45s).
	WorkerHeartbeatStaleFactor int
}

// Default returns the canonical liveness durations. These values reproduce
// the historical effective timings of all four detectors exactly; see the
// per-field doc comments for the old→new mapping.
func Default() Defaults {
	return Defaults{
		// staleinference (in-execution kill)
		IdleKill:             10 * time.Minute,
		IdleCheckInterval:    30 * time.Second,
		CompletedGracePeriod: time.Minute,

		// taskreaper (orphan reap)
		OrphanReaperGrace: 5 * time.Minute,
		ReaperInterval:    60 * time.Second,

		// operator (alerting)
		AlertCheckInterval:    60 * time.Second,
		LeaseExpiredGrace:     30 * time.Second,
		PendingTaskStuckAfter: 10 * time.Minute,
		MissingLabelAfter:     5 * time.Minute,

		// diagnose (forensic)
		AgentTimeout:         60 * time.Minute,
		ForensicOrphanFactor: 2,
		StuckIssueThreshold:  time.Hour,
		NoChildGracePeriod:   2 * time.Minute,

		WorkerHeartbeatStaleFactor: 3,
	}
}

// ForensicOrphanedAfterFor derives the forensic orphaned-after threshold for
// a task whose effective agent timeout is the supplied value. A zero or
// negative timeout falls back to AgentTimeout. This is the single derivation
// the forensic tool uses so its "2 × timeout" relationship lives in one place.
func (d Defaults) ForensicOrphanedAfterFor(agentTimeout time.Duration) time.Duration {
	if agentTimeout <= 0 {
		agentTimeout = d.AgentTimeout
	}
	factor := d.ForensicOrphanFactor
	if factor <= 0 {
		factor = 1
	}
	return time.Duration(factor) * agentTimeout
}

// WorkerHeartbeatStaleAfter derives the heartbeat-staleness window from a
// worker heartbeat interval.
func (d Defaults) WorkerHeartbeatStaleAfter(heartbeatInterval time.Duration) time.Duration {
	factor := d.WorkerHeartbeatStaleFactor
	if factor <= 0 {
		factor = 1
	}
	return time.Duration(factor) * heartbeatInterval
}

// Validate enforces the meaningful layering ordering invariants — each
// detector ordered against the budget it shares a clock with, so the
// detectors can never contradict the layer below them. (IdleKill and
// OrphanReaperGrace key off different signals on different hosts and are
// deliberately not ordered against each other; see the package doc.)
//
//	IdleKill <= AgentTimeout
//	    The in-execution watchdog must fire within the agent's own runtime
//	    budget, so a hung agent is killed before its policy timeout would
//	    have ended it. (This also backs the documented config invariant that
//	    an agent's policy.timeout must be >= idle_threshold.)
//
//	OrphanReaperGrace <= ForensicOrphanedAfter (with default AgentTimeout)
//	    The on-demand forensic tool must only flag tasks that the reaper
//	    has already had time to handle, so a "diagnose" report never
//	    contradicts an automatic reap that is still pending.
//
// A future edit that breaks either relationship fails this check (and the
// package test that calls it on Default()).
func (d Defaults) Validate() error {
	if d.IdleKill <= 0 || d.OrphanReaperGrace <= 0 || d.AgentTimeout <= 0 {
		return fmt.Errorf("liveness: durations must be positive (idle_kill=%s reaper_grace=%s agent_timeout=%s)",
			d.IdleKill, d.OrphanReaperGrace, d.AgentTimeout)
	}
	if d.IdleKill > d.AgentTimeout {
		return fmt.Errorf("liveness: ordering invariant violated: idle_kill (%s) must be <= agent_timeout (%s)",
			d.IdleKill, d.AgentTimeout)
	}
	forensic := d.ForensicOrphanedAfterFor(d.AgentTimeout)
	if d.OrphanReaperGrace > forensic {
		return fmt.Errorf("liveness: ordering invariant violated: orphan_reaper_grace (%s) must be <= forensic_orphaned_after (%s)",
			d.OrphanReaperGrace, forensic)
	}
	return nil
}
