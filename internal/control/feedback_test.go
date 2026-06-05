package control

import (
	"strings"
	"testing"
)

func TestFeedback_ComputesDelta(t *testing.T) {
	tests := []struct {
		name        string
		obj         *Objective
		obs         *Observation
		wantMissing []string
		wantMet     []string
		wantAllMet  bool
	}{
		{
			name: "all conditions met",
			obj: &Objective{
				BranchPushed:   true,
				PROpen:         true,
				LabelTarget:    "status:reviewing",
				IssueCommented: true,
			},
			obs: &Observation{
				BranchPushed:   true,
				PROpen:         true,
				PRNumber:       42,
				LabelCurrent:   "status:reviewing",
				IssueCommented: true,
			},
			wantMissing: nil,
			wantMet:     []string{"branch pushed", "PR open (#42)", "label is status:reviewing", "issue commented"},
			wantAllMet:  true,
		},
		{
			name: "nothing met",
			obj: &Objective{
				BranchPushed:   true,
				PROpen:         true,
				LabelTarget:    "status:reviewing",
				IssueCommented: true,
			},
			obs: &Observation{
				BranchPushed:   false,
				PROpen:         false,
				LabelCurrent:   "status:developing",
				IssueCommented: false,
			},
			wantMissing: []string{"branch not pushed", "PR not open", `label is "status:developing", want "status:reviewing"`, "no comment posted on issue"},
			wantMet:     nil,
			wantAllMet:  false,
		},
		{
			name: "partial - branch pushed but no PR",
			obj: &Objective{
				BranchPushed: true,
				PROpen:       true,
				LabelTarget:  "status:reviewing",
			},
			obs: &Observation{
				BranchPushed: true,
				PROpen:       false,
				LabelCurrent: "status:developing",
			},
			wantMissing: []string{"PR not open", `label is "status:developing", want "status:reviewing"`},
			wantMet:     []string{"branch pushed"},
			wantAllMet:  false,
		},
		{
			name: "merge objective - label met",
			obj:  MergeObjective(),
			obs: &Observation{
				LabelCurrent: "status:merged",
			},
			wantMissing: nil,
			wantMet:     []string{"label is status:merged"},
			wantAllMet:  true,
		},
		{
			name: "review objective - label still reviewing",
			obj:  ReviewObjective(),
			obs: &Observation{
				LabelCurrent:   "status:reviewing",
				IssueCommented: true,
			},
			wantMissing: []string{`label is "status:reviewing", want "status:merging"`},
			wantMet:     []string{"issue commented"},
			wantAllMet:  false,
		},
		{
			name: "empty objective - trivially met",
			obj:  &Objective{},
			obs: &Observation{
				BranchPushed: true,
				PROpen:       true,
				LabelCurrent: "status:anything",
			},
			wantMissing: nil,
			wantMet:     nil,
			wantAllMet:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			delta := Feedback(tc.obj, tc.obs)

			if tc.wantAllMet != delta.AllMet {
				t.Errorf("AllMet = %v, want %v", delta.AllMet, tc.wantAllMet)
			}

			if len(tc.wantMissing) != len(delta.Missing) {
				t.Fatalf("Missing count = %d, want %d\n  got:  %v\n  want: %v",
					len(delta.Missing), len(tc.wantMissing), delta.Missing, tc.wantMissing)
			}
			for i, want := range tc.wantMissing {
				if delta.Missing[i] != want {
					t.Errorf("Missing[%d] = %q, want %q", i, delta.Missing[i], want)
				}
			}

			if len(tc.wantMet) != len(delta.Met) {
				t.Fatalf("Met count = %d, want %d\n  got:  %v\n  want: %v",
					len(delta.Met), len(tc.wantMet), delta.Met, tc.wantMet)
			}
			for i, want := range tc.wantMet {
				if delta.Met[i] != want {
					t.Errorf("Met[%d] = %q, want %q", i, delta.Met[i], want)
				}
			}
		})
	}
}

func TestDelta_Message_ContainsContext(t *testing.T) {
	delta := &Delta{
		Met:     []string{"branch pushed"},
		Missing: []string{"PR not open", "label wrong"},
	}
	msg := delta.Message("Lincyaw/workbuddy", 42, "workbuddy/issue-42")

	if !strings.Contains(msg, "Lincyaw/workbuddy#42") {
		t.Errorf("message should contain repo#issue, got:\n%s", msg)
	}
	if !strings.Contains(msg, "workbuddy/issue-42") {
		t.Errorf("message should contain branch, got:\n%s", msg)
	}
	if !strings.Contains(msg, "[ok] branch pushed") {
		t.Errorf("message should contain met conditions, got:\n%s", msg)
	}
	if !strings.Contains(msg, "[MISSING] PR not open") {
		t.Errorf("message should contain missing conditions, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Please address") {
		t.Errorf("message should contain action prompt, got:\n%s", msg)
	}
}
