// Tests for PR-branch → parent-issue-number derivation (REQ-138 / #320).

package poller

import "testing"

func TestParentIssueFromBranch(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"workbuddy/issue-42", 42},
		{"workbuddy/issue-1", 1},
		{"claude/issue-7", 7},
		{"codex/issue-100", 100},
		{"workbuddy/issue-42-followup", 42},
		{"issue-9", 9},
		{"feature/foo", 0},
		{"main", 0},
		{"", 0},
		{"workbuddy/issue-", 0},
		{"workbuddy/issue-abc", 0},
	}
	for _, c := range cases {
		got := parentIssueFromBranch(c.in)
		if got != c.want {
			t.Errorf("parentIssueFromBranch(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
