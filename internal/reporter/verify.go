package reporter

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/ghadapter"
)

// ClaimType identifies the category of a side-effect claim.
type ClaimType string

const (
	ClaimCommentPR    ClaimType = "comment_pr"
	ClaimCommentIssue ClaimType = "comment_issue"
	ClaimLabels       ClaimType = "labels"
	ClaimPRCreated    ClaimType = "pr_created"
	ClaimBranchPushed ClaimType = "branch_pushed"
	ClaimFileCreated  ClaimType = "file_created"
	ClaimCommit       ClaimType = "commit"
)

// ClaimCheck records a single claim and its verified reality.
type ClaimCheck struct {
	Type   ClaimType
	Claim  string
	Actual string
	OK     bool
}

// VerificationResult is the outcome of scanning an agent output for claims
// and verifying each one against the actual world state.
type VerificationResult struct {
	Partial bool
	Checks  []ClaimCheck
}

// VerificationInput describes the agent output and run window that produced it.
type VerificationInput struct {
	Output    string
	StartedAt time.Time
	EndedAt   time.Time
}

// ClaimVerifier inspects agent output for claims about external side-effects
// and verifies them against the live GitHub / git state.
type ClaimVerifier interface {
	Verify(ctx context.Context, repo string, issueNum int, input VerificationInput) (*VerificationResult, error)
}

// GHClaimVerifier implements ClaimVerifier using the gh CLI and git.
type GHClaimVerifier struct {
	runCommand func(ctx context.Context, name string, args ...string) ([]byte, error)
	gh         *ghadapter.CLI
	loginOnce  sync.Once
	login      string
	loginErr   error
}

const commentTimestampSkew = 2 * time.Minute

// NewGHClaimVerifier creates a GHClaimVerifier that shells out to gh/git.
func NewGHClaimVerifier() *GHClaimVerifier {
	v := &GHClaimVerifier{}
	v.runCommand = v.defaultRunCommand
	v.gh = ghadapter.NewCLIWithRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return v.runCommand(ctx, name, args...)
	})
	return v
}

func (v *GHClaimVerifier) authenticatedLogin(ctx context.Context) (string, error) {
	v.loginOnce.Do(func() {
		out, err := v.ghClient().AuthenticatedLogin(ctx)
		if err != nil {
			v.loginErr = err
			return
		}
		v.login = strings.TrimSpace(out)
	})
	return v.login, v.loginErr
}

func (v *GHClaimVerifier) defaultRunCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func (v *GHClaimVerifier) ghClient() *ghadapter.CLI {
	if v != nil && v.gh != nil {
		return v.gh
	}
	return ghadapter.NewCLIWithRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return v.runCommand(ctx, name, args...)
	})
}

var (
	commentOnPRPattern    = regexp.MustCompile(`(?i)(?:posted|added|wrote)\b.*?comment\b.*?on\b.*?pr\b.*?#?(\b\d+\b)`)
	commentOnIssuePattern = regexp.MustCompile(`(?i)(?:posted|added|wrote)\b.*?comment\b.*?on\b.*?issue\b.*?#?(\b\d+\b)`)
	labelUpdatePattern    = regexp.MustCompile(`(?i)(?:updated|changed|flipped)\b.*?(?:the\b)?.*?labels`)
	labelAddedPattern     = regexp.MustCompile(`(?i)(?:added|flipped\b.*?to|updated\b.*?to)\b.*?status:([a-zA-Z0-9_-]+)`)
	labelRemovedPattern   = regexp.MustCompile(`(?i)removed\b.*?status:([a-zA-Z0-9_-]+)`)
	prCreatedPattern      = regexp.MustCompile(`(?i)(?:created|opened)\b.*?pr\b.*?#?(\d+)`)
	branchPushedPattern   = regexp.MustCompile("(?i)(?:pushed|created)\\b.*?branch\\b\\s+([`\"']?\\S+[`\"']?)")
	fileCreatedPattern    = regexp.MustCompile("(?i)(?:created|added)\\b.*?file\\b\\s+([`\"']?\\S+[`\"']?)")
	commitPattern         = regexp.MustCompile(`(?i)\bcommitted\b(?:\s+["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]|\s+([0-9a-f]{7,40})|\s+([^\n.;]+))`)
)

// Verify scans agent output for known claim patterns and runs the
// corresponding gh/git checks.
func (v *GHClaimVerifier) Verify(ctx context.Context, repo string, issueNum int, input VerificationInput) (*VerificationResult, error) {
	output := strings.TrimSpace(input.Output)
	if strings.TrimSpace(output) == "" {
		return &VerificationResult{}, nil
	}

	var checks []ClaimCheck

	for _, m := range commentOnPRPattern.FindAllStringSubmatch(output, -1) {
		prNum, _ := strconv.Atoi(m[1])
		checks = append(checks, v.verifyCommentOnPR(ctx, repo, prNum, input))
	}

	for _, m := range commentOnIssuePattern.FindAllStringSubmatch(output, -1) {
		num, _ := strconv.Atoi(m[1])
		checks = append(checks, v.verifyCommentOnIssue(ctx, repo, num, input))
	}

	labelClaims := false
	for _, m := range labelAddedPattern.FindAllStringSubmatch(output, -1) {
		labelClaims = true
		checks = append(checks, v.verifyLabelPresent(ctx, repo, issueNum, "status:"+m[1]))
	}
	for _, m := range labelRemovedPattern.FindAllStringSubmatch(output, -1) {
		labelClaims = true
		checks = append(checks, v.verifyLabelAbsent(ctx, repo, issueNum, "status:"+m[1]))
	}
	if !labelClaims && labelUpdatePattern.MatchString(output) {
		checks = append(checks, v.verifyLabelsGeneric(ctx, repo, issueNum))
	}

	for _, m := range prCreatedPattern.FindAllStringSubmatch(output, -1) {
		prNum, _ := strconv.Atoi(m[1])
		checks = append(checks, v.verifyPRCreated(ctx, repo, prNum))
	}

	for _, m := range branchPushedPattern.FindAllStringSubmatch(output, -1) {
		checks = append(checks, v.verifyBranchPushed(ctx, repo, normalizeClaimToken(m[1])))
	}

	for _, m := range fileCreatedPattern.FindAllStringSubmatch(output, -1) {
		checks = append(checks, v.verifyFileCreated(ctx, repo, normalizeClaimToken(m[1])))
	}
	for _, m := range commitPattern.FindAllStringSubmatch(output, -1) {
		claim := firstNonEmpty(m[1], m[2], m[3])
		checks = append(checks, v.verifyCommitClaim(ctx, normalizeClaimToken(claim)))
	}

	partial := false
	for _, c := range checks {
		if !c.OK {
			partial = true
			break
		}
	}
	return &VerificationResult{Partial: partial, Checks: checks}, nil
}

func (v *GHClaimVerifier) verifyCommentOnPR(ctx context.Context, repo string, prNum int, input VerificationInput) ClaimCheck {
	claim := fmt.Sprintf("posted comment on PR #%d", prNum)
	login, err := v.authenticatedLogin(ctx)
	if err != nil {
		return ClaimCheck{Type: ClaimCommentPR, Claim: claim, Actual: err.Error(), OK: false}
	}
	comments, err := v.ghClient().ReadPullRequestComments(ctx, repo, prNum)
	if err != nil {
		return ClaimCheck{Type: ClaimCommentPR, Claim: claim, Actual: err.Error(), OK: false}
	}
	if len(comments) == 0 {
		return ClaimCheck{Type: ClaimCommentPR, Claim: claim, Actual: "no comments on PR", OK: false}
	}
	last := comments[len(comments)-1]
	if login != "" && last.Author != login {
		return ClaimCheck{Type: ClaimCommentPR, Claim: claim, Actual: fmt.Sprintf("most recent comment author is %s, expected %s", last.Author, login), OK: false}
	}
	if !commentWithinRunWindow(last.CreatedAt, input) {
		return ClaimCheck{Type: ClaimCommentPR, Claim: claim, Actual: fmt.Sprintf("most recent comment by %s is outside this run window (%s)", last.Author, last.CreatedAt.Format(time.RFC3339)), OK: false}
	}
	return ClaimCheck{Type: ClaimCommentPR, Claim: claim, Actual: fmt.Sprintf("recent comment by %s at %s", last.Author, last.CreatedAt.Format(time.RFC3339)), OK: true}
}

func (v *GHClaimVerifier) verifyCommentOnIssue(ctx context.Context, repo string, issueNum int, input VerificationInput) ClaimCheck {
	claim := fmt.Sprintf("posted comment on issue #%d", issueNum)
	login, err := v.authenticatedLogin(ctx)
	if err != nil {
		return ClaimCheck{Type: ClaimCommentIssue, Claim: claim, Actual: err.Error(), OK: false}
	}
	comments, err := v.ghClient().ReadDetailedIssueComments(ctx, repo, issueNum)
	if err != nil {
		return ClaimCheck{Type: ClaimCommentIssue, Claim: claim, Actual: err.Error(), OK: false}
	}
	if len(comments) == 0 {
		return ClaimCheck{Type: ClaimCommentIssue, Claim: claim, Actual: "no comments on issue", OK: false}
	}
	last := comments[len(comments)-1]
	if login != "" && last.Author != login {
		return ClaimCheck{Type: ClaimCommentIssue, Claim: claim, Actual: fmt.Sprintf("most recent comment author is %s, expected %s", last.Author, login), OK: false}
	}
	if !commentWithinRunWindow(last.CreatedAt, input) {
		return ClaimCheck{Type: ClaimCommentIssue, Claim: claim, Actual: fmt.Sprintf("most recent comment by %s is outside this run window (%s)", last.Author, last.CreatedAt.Format(time.RFC3339)), OK: false}
	}
	return ClaimCheck{Type: ClaimCommentIssue, Claim: claim, Actual: fmt.Sprintf("recent comment by %s at %s", last.Author, last.CreatedAt.Format(time.RFC3339)), OK: true}
}

func (v *GHClaimVerifier) verifyLabelPresent(ctx context.Context, repo string, issueNum int, label string) ClaimCheck {
	claim := fmt.Sprintf("added %s", label)
	labels, err := v.ghClient().ReadIssueLabels(repo, issueNum)
	if err != nil {
		return ClaimCheck{Type: ClaimLabels, Claim: claim, Actual: err.Error(), OK: false}
	}
	for _, current := range labels {
		if current == label {
			return ClaimCheck{Type: ClaimLabels, Claim: claim, Actual: fmt.Sprintf("label present: %s", label), OK: true}
		}
	}
	return ClaimCheck{Type: ClaimLabels, Claim: claim, Actual: fmt.Sprintf("label not found in current labels: %s", strings.Join(labels, ", ")), OK: false}
}

func (v *GHClaimVerifier) verifyLabelAbsent(ctx context.Context, repo string, issueNum int, label string) ClaimCheck {
	claim := fmt.Sprintf("removed %s", label)
	labels, err := v.ghClient().ReadIssueLabels(repo, issueNum)
	if err != nil {
		return ClaimCheck{Type: ClaimLabels, Claim: claim, Actual: err.Error(), OK: false}
	}
	for _, current := range labels {
		if current == label {
			return ClaimCheck{Type: ClaimLabels, Claim: claim, Actual: fmt.Sprintf("label still present: %s", label), OK: false}
		}
	}
	return ClaimCheck{Type: ClaimLabels, Claim: claim, Actual: fmt.Sprintf("label absent: %s", label), OK: true}
}

func (v *GHClaimVerifier) verifyLabelsGeneric(ctx context.Context, repo string, issueNum int) ClaimCheck {
	claim := "updated labels"
	labels, err := v.ghClient().ReadIssueLabels(repo, issueNum)
	if err != nil {
		return ClaimCheck{Type: ClaimLabels, Claim: claim, Actual: err.Error(), OK: false}
	}
	actual := fmt.Sprintf("current labels: %s", strings.Join(labels, ", "))
	if len(labels) == 0 {
		actual = "current labels: none"
	}
	return ClaimCheck{
		Type:   ClaimLabels,
		Claim:  claim,
		Actual: actual + "; generic label update claim does not specify an intended label set",
		OK:     false,
	}
}

func (v *GHClaimVerifier) verifyPRCreated(ctx context.Context, repo string, prNum int) ClaimCheck {
	claim := fmt.Sprintf("created PR #%d", prNum)
	pr, err := v.ghClient().ReadPullRequest(ctx, repo, prNum)
	if err != nil {
		return ClaimCheck{Type: ClaimPRCreated, Claim: claim, Actual: err.Error(), OK: false}
	}
	if pr.State != "OPEN" && pr.State != "open" {
		return ClaimCheck{Type: ClaimPRCreated, Claim: claim, Actual: fmt.Sprintf("PR state is %s", pr.State), OK: false}
	}
	return ClaimCheck{Type: ClaimPRCreated, Claim: claim, Actual: fmt.Sprintf("PR exists, state=%s, branch=%s", pr.State, pr.HeadRefName), OK: true}
}

func (v *GHClaimVerifier) verifyBranchPushed(ctx context.Context, repo string, branch string) ClaimCheck {
	claim := fmt.Sprintf("pushed branch %s", branch)
	remoteOut, err := v.runCommand(ctx, "git", "ls-remote", "origin", branch)
	if err != nil {
		return ClaimCheck{Type: ClaimBranchPushed, Claim: claim, Actual: fmt.Sprintf("git error: %s", string(remoteOut)), OK: false}
	}
	remoteOut = bytesTrimSpace(remoteOut)
	if len(remoteOut) == 0 {
		return ClaimCheck{Type: ClaimBranchPushed, Claim: claim, Actual: "branch not found on origin", OK: false}
	}
	localOut, err := v.runCommand(ctx, "git", "rev-parse", branch)
	if err != nil {
		return ClaimCheck{Type: ClaimBranchPushed, Claim: claim, Actual: fmt.Sprintf("local branch lookup failed: %s", string(localOut)), OK: false}
	}
	localSHA := strings.TrimSpace(string(localOut))
	remoteSHA := strings.Fields(string(remoteOut))[0]
	if localSHA == "" {
		return ClaimCheck{Type: ClaimBranchPushed, Claim: claim, Actual: "local branch has no resolvable tip", OK: false}
	}
	if remoteSHA != localSHA {
		return ClaimCheck{
			Type:   ClaimBranchPushed,
			Claim:  claim,
			Actual: fmt.Sprintf("remote tip %s does not match local %s", remoteSHA, localSHA),
			OK:     false,
		}
	}
	return ClaimCheck{Type: ClaimBranchPushed, Claim: claim, Actual: fmt.Sprintf("branch exists on origin at %s", remoteSHA), OK: true}
}

func (v *GHClaimVerifier) verifyFileCreated(ctx context.Context, repo string, path string) ClaimCheck {
	claim := fmt.Sprintf("created file %s", path)
	out, err := v.runCommand(ctx, "git", "log", "-1", "--name-only", "--format=")
	if err != nil {
		return ClaimCheck{Type: ClaimFileCreated, Claim: claim, Actual: fmt.Sprintf("git error: %s", string(out)), OK: false}
	}
	files := strings.Split(string(out), "\n")
	for _, f := range files {
		if strings.TrimSpace(f) == path {
			return ClaimCheck{Type: ClaimFileCreated, Claim: claim, Actual: fmt.Sprintf("file found in last commit: %s", path), OK: true}
		}
	}
	return ClaimCheck{Type: ClaimFileCreated, Claim: claim, Actual: "file not in last commit", OK: false}
}

func (v *GHClaimVerifier) verifyCommitClaim(ctx context.Context, claimText string) ClaimCheck {
	claim := fmt.Sprintf("committed %s", claimText)
	out, err := v.runCommand(ctx, "git", "log", "-1", "--format=%H%n%s")
	if err != nil {
		return ClaimCheck{Type: ClaimCommit, Claim: claim, Actual: fmt.Sprintf("git error: %s", string(out)), OK: false}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return ClaimCheck{Type: ClaimCommit, Claim: claim, Actual: "no commit found", OK: false}
	}
	sha := strings.TrimSpace(lines[0])
	subject := ""
	if len(lines) > 1 {
		subject = strings.TrimSpace(lines[1])
	}
	if commitClaimMatches(claimText, sha, subject) {
		return ClaimCheck{Type: ClaimCommit, Claim: claim, Actual: fmt.Sprintf("latest commit %s %q", shortSHA(sha), subject), OK: true}
	}
	return ClaimCheck{
		Type:   ClaimCommit,
		Claim:  claim,
		Actual: fmt.Sprintf("latest commit is %s %q", shortSHA(sha), subject),
		OK:     false,
	}
}

func normalizeClaimToken(raw string) string {
	return strings.Trim(strings.TrimSpace(raw), "`\"'.,:;)]}")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func commitClaimMatches(claim, sha, subject string) bool {
	claim = strings.TrimSpace(claim)
	if claim == "" {
		return false
	}
	lowerClaim := strings.ToLower(claim)
	lowerSHA := strings.ToLower(sha)
	lowerSubject := strings.ToLower(subject)
	return strings.HasPrefix(lowerSHA, lowerClaim) || strings.Contains(lowerSubject, lowerClaim)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func commentWithinRunWindow(createdAt time.Time, input VerificationInput) bool {
	if createdAt.IsZero() {
		return false
	}
	end := input.EndedAt
	start := input.StartedAt
	if end.IsZero() {
		end = time.Now()
	}
	if start.IsZero() {
		start = end.Add(-30 * time.Minute)
	}
	start = start.Add(-commentTimestampSkew)
	end = end.Add(commentTimestampSkew)
	return !createdAt.Before(start) && !createdAt.After(end)
}
