package control

import (
	"fmt"
	"sort"
	"strings"
)

// Action is what the controller decides to do.
type Action int

const (
	ActionResume   Action = iota // resume same session with feedback
	ActionBlock                  // mark blocked, stop trying
	ActionComplete               // all conditions met, done
)

// Decision is the controller's output.
type Decision struct {
	Action  Action
	Message string // feedback message for resume, or block reason
	Round   int    // current round number
}

// Controller implements the control policy.
type Controller struct {
	MaxRounds int // default 5
}

// Decide takes the current delta and round count, plus the previous delta
// (nil on the first observation), and returns an action.
func (c *Controller) Decide(delta *Delta, round int, prevDelta *Delta) *Decision {
	if delta.AllMet {
		return &Decision{Action: ActionComplete, Round: round}
	}

	maxRounds := c.MaxRounds
	if maxRounds <= 0 {
		maxRounds = 5
	}

	if round >= maxRounds {
		return &Decision{
			Action:  ActionBlock,
			Message: fmt.Sprintf("exhausted %d control rounds; unmet: %s", round, strings.Join(delta.Missing, ", ")),
			Round:   round,
		}
	}

	// Detect stuck state: same unmet conditions as the previous round, and
	// we've been trying for at least 3 rounds.
	if prevDelta != nil && sameUnmet(delta, prevDelta) && round >= 3 {
		return &Decision{
			Action:  ActionBlock,
			Message: fmt.Sprintf("stuck: same conditions unmet for %d rounds: %s", round, strings.Join(delta.Missing, ", ")),
			Round:   round,
		}
	}

	return &Decision{
		Action: ActionResume,
		Round:  round,
	}
}

// sameUnmet returns true if both deltas have the same set of missing
// conditions (order-independent).
func sameUnmet(a, b *Delta) bool {
	if len(a.Missing) != len(b.Missing) {
		return false
	}
	sa := make([]string, len(a.Missing))
	copy(sa, a.Missing)
	sort.Strings(sa)

	sb := make([]string, len(b.Missing))
	copy(sb, b.Missing)
	sort.Strings(sb)

	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}
