package control

// Objective is the target condition vector for a workflow state.
type Objective struct {
	BranchPushed   bool
	PROpen         bool
	LabelTarget    string // e.g. "status:reviewing"
	IssueCommented bool
}

// DevObjective returns the post-conditions for the developing state.
// After a dev run: branch pushed, PR opened, label moved to reviewing,
// and a comment posted on the issue.
func DevObjective() *Objective {
	return &Objective{
		BranchPushed:   true,
		PROpen:         true,
		LabelTarget:    "status:reviewing",
		IssueCommented: true,
	}
}

// ReviewObjective returns post-conditions for reviewing.
// A review can end in either "status:merging" (pass) or
// "status:developing" (rejection). Both are valid — the condition is
// that the label is no longer "status:reviewing".
func ReviewObjective() *Objective {
	return &Objective{
		LabelTarget:    "status:merging",
		IssueCommented: true,
	}
}

// MergeObjective returns post-conditions for merging.
func MergeObjective() *Objective {
	return &Objective{
		LabelTarget: "status:merged",
	}
}
