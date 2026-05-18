package dependency

import (
	"fmt"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// GateStore is the narrow store surface needed to evaluate a dispatch gate.
type GateStore interface {
	QueryIssueDependencyState(repo string, issueNum int) (*store.IssueDependencyState, error)
}

// IsBlocked reports whether dispatch for (repo, issueNum) is currently
// blocked by its recorded dependency verdict, and returns the observed
// verdict string for caller-side telemetry. A verdict of "blocked" or
// "needs_human" gates the dispatch; anything else (ready/override, or no
// recorded state yet) permits it.
//
// Return contract:
//   - When no row is recorded (or the gate store is nil): (false, "", nil).
//   - When the underlying read errors: (false, "", err) — verdict is left
//     empty because nothing was observed.
//   - Otherwise: (gated?, verdict, nil) where verdict is the raw
//     store.DependencyVerdict* string the row holds.
//
// This is the single source of truth for the dispatch-gate policy: both
// the state machine and the router consume it via this helper rather than
// inlining the verdict switch (REQ-149 / #345 W2 cleanup).
func IsBlocked(st GateStore, repo string, issueNum int) (blocked bool, verdict string, err error) {
	if st == nil {
		return false, "", nil
	}
	depState, qerr := st.QueryIssueDependencyState(repo, issueNum)
	if qerr != nil {
		return false, "", fmt.Errorf("query dependency state for %s#%d: %w", repo, issueNum, qerr)
	}
	if depState == nil {
		return false, "", nil
	}
	switch depState.Verdict {
	case store.DependencyVerdictBlocked, store.DependencyVerdictNeedsHuman:
		return true, depState.Verdict, nil
	default:
		return false, depState.Verdict, nil
	}
}
