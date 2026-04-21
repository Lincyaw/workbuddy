package ghadapter

import (
	"context"
	"strings"
	"testing"
)

func TestListPRsUsesOpenStateContract(t *testing.T) {
	t.Parallel()

	var gotArgs []string
	cli := NewCLIWithRunner(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "gh" {
			t.Fatalf("name = %q, want gh", name)
		}
		gotArgs = append([]string(nil), args...)
		return []byte(`[{"number":17,"url":"https://github.com/owner/repo/pull/17","headRefName":"feat/test","state":"OPEN"}]`), nil
	})

	prs, err := cli.ListPRs("owner/repo")
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 17 {
		t.Fatalf("prs = %#v, want one PR", prs)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--state open") {
		t.Fatalf("ListPRs args = %q, want --state open", args)
	}
	if strings.Contains(args, "--state all") {
		t.Fatalf("ListPRs args = %q, must not request historical PRs", args)
	}
}

func TestReadIssueNormalizesStateFields(t *testing.T) {
	t.Parallel()

	cli := NewCLIWithRunner(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "gh" {
			t.Fatalf("name = %q, want gh", name)
		}
		return []byte(`{
			"data":{
				"repository":{
					"issue":{
						"number":42,
						"state":"CLOSED",
						"stateReason":"COMPLETED",
						"body":"done",
						"labels":{"nodes":[{"name":"status:reviewed"}]},
						"closedByPullRequestsReferences":{"nodes":[{"number":9,"state":"MERGED","url":"https://github.com/owner/repo/pull/9"}]}
					}
				}
			}
		}`), nil
	})

	issue, err := cli.ReadIssue("owner/repo", 42)
	if err != nil {
		t.Fatalf("ReadIssue: %v", err)
	}
	if issue.State != "closed" {
		t.Fatalf("State = %q, want closed", issue.State)
	}
	if issue.StateReason != "completed" {
		t.Fatalf("StateReason = %q, want completed", issue.StateReason)
	}
	if !issue.ClosedByLinkedPR {
		t.Fatal("ClosedByLinkedPR = false, want true")
	}
}
