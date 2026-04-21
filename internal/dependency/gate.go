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
// blocked by its recorded dependency verdict. A verdict of "blocked" or
// "needs_human" gates the dispatch; anything else (ready/override, or no
// recorded state yet) permits it.
//
// This mirrors the policy previously inlined in internal/router/router.go so
// the scheduling layer no longer reaches into store verdict semantics
// directly.
func IsBlocked(st GateStore, repo string, issueNum int) (bool, error) {
	if st == nil {
		return false, nil
	}
	depState, err := st.QueryIssueDependencyState(repo, issueNum)
	if err != nil {
		return false, fmt.Errorf("query dependency state for %s#%d: %w", repo, issueNum, err)
	}
	if depState == nil {
		return false, nil
	}
	switch depState.Verdict {
	case store.DependencyVerdictBlocked, store.DependencyVerdictNeedsHuman:
		return true, nil
	default:
		return false, nil
	}
}
