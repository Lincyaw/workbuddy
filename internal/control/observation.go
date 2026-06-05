package control

import (
	"context"
)

// Observation is the measured state after an agent run.
type Observation struct {
	BranchPushed   bool   // remote has the branch
	PROpen         bool   // at least one open PR for the branch
	PRNumber       int    // PR number if open (0 if none)
	LabelCurrent   string // current status:* label on the issue
	IssueCommented bool   // agent posted a comment this round
}

// Observer checks the actual world state via git/GitHub API.
// Runs on the workbuddy pod (NOT inside the sandbox).
type Observer interface {
	Observe(ctx context.Context, repo string, issueNum int, branch string) (*Observation, error)
}
