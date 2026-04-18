package reporter

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type mockCommandRunner struct {
	responses map[string]string
	errors    map[string]error
}

func (m *mockCommandRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + fmt.Sprint(args)
	if err, ok := m.errors[key]; ok {
		return nil, err
	}
	if resp, ok := m.responses[key]; ok {
		return []byte(resp), nil
	}
	return nil, fmt.Errorf("unexpected command: %s %v", name, args)
}

func fixedCommentJSON(author string, createdAt time.Time) string {
	return fmt.Sprintf(`{"comments":[{"author":{"login":"%s"},"createdAt":"%s","body":"test"}]}`, author, createdAt.Format(time.RFC3339))
}

func verificationInput(output string, end time.Time) VerificationInput {
	return VerificationInput{
		Output:    output,
		StartedAt: end.Add(-5 * time.Minute),
		EndedAt:   end,
	}
}

func emptyCommentsJSON() string {
	return `{"comments":[]}`
}

func labelsJSON(names ...string) string {
	var items string
	for i, n := range names {
		if i > 0 {
			items += ","
		}
		items += fmt.Sprintf(`{"name":"%s"}`, n)
	}
	return fmt.Sprintf(`{"labels":[%s]}`, items)
}

func TestVerify_AllClaimsVerified(t *testing.T) {
	now := time.Now().UTC()
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [api user --jq .login]":                         "bot\n",
			"gh [pr view 4 --repo owner/repo --json comments]":  fixedCommentJSON("bot", now),
			"gh [issue view 1 --repo owner/repo --json labels]": labelsJSON("status:done"),
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I posted a review comment on PR #4 and flipped labels to status:done"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, now))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Partial {
		t.Fatalf("expected success, got partial: %+v", res.Checks)
	}
	if len(res.Checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(res.Checks))
	}
}

func TestVerify_CommentMissing(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [api user --jq .login]":                        "bot\n",
			"gh [pr view 4 --repo owner/repo --json comments]": emptyCommentsJSON(),
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I posted a review comment on PR #4"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Partial {
		t.Fatal("expected partial, got success")
	}
	if len(res.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(res.Checks))
	}
	if res.Checks[0].OK {
		t.Fatal("expected comment check to fail")
	}
}

func TestVerify_LabelsNotFlipped(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [issue view 1 --repo owner/repo --json labels]": labelsJSON("status:reviewing"),
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I flipped labels to status:done"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Partial {
		t.Fatal("expected partial, got success")
	}
	if len(res.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(res.Checks))
	}
	if res.Checks[0].OK {
		t.Fatal("expected label check to fail")
	}
}

func TestVerify_UpdatedLabelsToStatus(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [issue view 1 --repo owner/repo --json labels]": labelsJSON("status:done"),
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I updated labels to status:done"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Partial {
		t.Fatalf("expected success, got partial: %+v", res.Checks)
	}
	if len(res.Checks) != 1 || !res.Checks[0].OK {
		t.Fatalf("expected one successful label check, got %+v", res.Checks)
	}
}

func TestVerify_GenericUpdatedLabelsIsUnverifiable(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [issue view 1 --repo owner/repo --json labels]": labelsJSON("status:reviewing"),
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I updated labels"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Partial {
		t.Fatal("expected generic label claim to be partial")
	}
	if len(res.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(res.Checks))
	}
	if !strings.Contains(res.Checks[0].Actual, "does not specify an intended label set") {
		t.Fatalf("unexpected actual detail: %q", res.Checks[0].Actual)
	}
}

func TestVerify_NoClaims(t *testing.T) {
	v := &GHClaimVerifier{runCommand: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("should not be called")
	}}
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput("I did some internal thinking", time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Partial {
		t.Fatal("expected no partial when no claims made")
	}
	if len(res.Checks) != 0 {
		t.Fatalf("expected 0 checks, got %d", len(res.Checks))
	}
}

func TestVerify_PRCommentOnIssue(t *testing.T) {
	now := time.Now().UTC()
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [api user --jq .login]":                           "bot\n",
			"gh [issue view 2 --repo owner/repo --json comments]": fixedCommentJSON("bot", now),
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I added a comment on issue #2"
	res, err := v.Verify(context.Background(), "owner/repo", 2, verificationInput(output, now))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Partial {
		t.Fatalf("expected success, got partial: %+v", res.Checks)
	}
}

func TestVerify_PRCreated(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [pr view 5 --repo owner/repo --json state,headRefName]": `{"state":"OPEN","headRefName":"feature-branch"}`,
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I created PR #5"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Partial {
		t.Fatalf("expected success, got partial: %+v", res.Checks)
	}
	if len(res.Checks) != 1 || res.Checks[0].Type != ClaimPRCreated {
		t.Fatalf("expected PR-created check, got %+v", res.Checks)
	}
}

func TestVerify_BranchPushed(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"git [ls-remote origin feature-branch]": "abc123\trefs/heads/feature-branch",
			"git [rev-parse feature-branch]":        "abc123\n",
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I pushed branch feature-branch"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Partial {
		t.Fatalf("expected success, got partial: %+v", res.Checks)
	}
	if len(res.Checks) != 1 || res.Checks[0].Type != ClaimBranchPushed {
		t.Fatalf("expected branch-pushed check, got %+v", res.Checks)
	}
}

func TestVerify_BranchPushedTipMismatch(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"git [ls-remote origin feature-branch]": "abc123\trefs/heads/feature-branch",
			"git [rev-parse feature-branch]":        "def456\n",
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I pushed branch feature-branch"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Partial {
		t.Fatal("expected partial, got success")
	}
	if len(res.Checks) != 1 || !strings.Contains(res.Checks[0].Actual, "does not match local") {
		t.Fatalf("unexpected checks: %+v", res.Checks)
	}
}

func TestVerify_FileCreated(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"git [log -1 --name-only --format=]": "README.md\ninternal/reporter/verify.go\n",
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I created file internal/reporter/verify.go"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Partial {
		t.Fatalf("expected success, got partial: %+v", res.Checks)
	}
	if len(res.Checks) != 1 || res.Checks[0].Type != ClaimFileCreated {
		t.Fatalf("expected file-created check, got %+v", res.Checks)
	}
}

func TestVerify_CommitClaimBySubject(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"git [log -1 --format=%H%n%s]": "0123456789abcdef0123456789abcdef01234567\nFix issue #118 verification\n",
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := `I committed "Fix issue #118 verification"`
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Partial {
		t.Fatalf("expected success, got partial: %+v", res.Checks)
	}
	if len(res.Checks) != 1 || res.Checks[0].Type != ClaimCommit {
		t.Fatalf("expected commit check, got %+v", res.Checks)
	}
}

func TestVerify_CommitClaimMissing(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"git [log -1 --format=%H%n%s]": "fedcba9876543210fedcba9876543210fedcba98\nDifferent subject\n",
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := `I committed "Fix issue #118 verification"`
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Partial {
		t.Fatal("expected partial, got success")
	}
	if len(res.Checks) != 1 || !strings.Contains(res.Checks[0].Actual, "latest commit is") {
		t.Fatalf("unexpected checks: %+v", res.Checks)
	}
}

func TestVerify_LabelRemoved(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [issue view 1 --repo owner/repo --json labels]": labelsJSON("status:reviewing"),
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I removed status:developing"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Partial {
		t.Fatalf("expected success, got partial: %+v", res.Checks)
	}
}

func TestVerify_LabelRemovedStillPresent(t *testing.T) {
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [issue view 1 --repo owner/repo --json labels]": labelsJSON("status:developing", "status:reviewing"),
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I removed status:developing"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, time.Now().UTC()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Partial {
		t.Fatal("expected partial, got success")
	}
}

func TestVerify_CommentWrongAuthor(t *testing.T) {
	now := time.Now().UTC()
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [api user --jq .login]":                        "bot\n",
			"gh [pr view 4 --repo owner/repo --json comments]": fixedCommentJSON("someone-else", now),
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I posted a review comment on PR #4"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, now))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Partial {
		t.Fatal("expected partial, got success")
	}
	if len(res.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(res.Checks))
	}
	if res.Checks[0].OK {
		t.Fatal("expected comment check to fail")
	}
	if got := res.Checks[0].Actual; got == "" || !strings.Contains(got, "expected bot") {
		t.Fatalf("unexpected actual detail: %q", got)
	}
}

func TestVerify_CommentOutsideRunWindow(t *testing.T) {
	end := time.Now().UTC()
	oldComment := end.Add(-20 * time.Minute)
	runner := &mockCommandRunner{
		responses: map[string]string{
			"gh [api user --jq .login]":                        "bot\n",
			"gh [pr view 4 --repo owner/repo --json comments]": fixedCommentJSON("bot", oldComment),
		},
	}
	v := &GHClaimVerifier{runCommand: runner.run}
	output := "I posted a review comment on PR #4"
	res, err := v.Verify(context.Background(), "owner/repo", 1, verificationInput(output, end))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Partial {
		t.Fatal("expected partial, got success")
	}
	if len(res.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(res.Checks))
	}
	if !strings.Contains(res.Checks[0].Actual, "outside this run window") {
		t.Fatalf("unexpected actual detail: %q", res.Checks[0].Actual)
	}
}
