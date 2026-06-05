package control

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ghCallTimeout is the per-call timeout for each gh CLI invocation.
const ghCallTimeout = 30 * time.Second

// GHObserver implements Observer via the gh CLI.
// It runs on the workbuddy pod and uses the pod's GH_TOKEN.
type GHObserver struct{}

var _ Observer = (*GHObserver)(nil)

func (o *GHObserver) Observe(ctx context.Context, repo string, issueNum int, branch string) (*Observation, error) {
	obs := &Observation{}

	// 1. Check if branch exists on remote.
	// A 404 (exit code 1 with "Not Found") is not an error — branch simply
	// does not exist yet. Any other failure is propagated.
	branchOut, err := ghExecWithTimeout(ctx, ghCallTimeout, "api",
		fmt.Sprintf("repos/%s/branches/%s", repo, branch),
		"--silent", "--jq", ".name")
	if err != nil {
		if !isGHNotFound(err) {
			return nil, fmt.Errorf("observe branch: %w", err)
		}
		// 404 → branch does not exist, BranchPushed stays false.
	} else if strings.TrimSpace(branchOut) != "" {
		obs.BranchPushed = true
	}

	// 2. Check for open PR targeting this branch.
	prOut, err := ghExecWithTimeout(ctx, ghCallTimeout, "pr", "list",
		"-R", repo,
		"--head", branch,
		"--state", "open",
		"--json", "number",
		"--jq", ".[0].number")
	if err != nil {
		return nil, fmt.Errorf("observe PR: %w", err)
	}
	if n, perr := strconv.Atoi(strings.TrimSpace(prOut)); perr == nil && n > 0 {
		obs.PROpen = true
		obs.PRNumber = n
	}

	// 3. Get current status:* label on the issue.
	labelOut, err := ghExecWithTimeout(ctx, ghCallTimeout, "issue", "view",
		strconv.Itoa(issueNum),
		"-R", repo,
		"--json", "labels",
		"--jq", `[.labels[].name] | map(select(startswith("status:")))[0]`)
	if err != nil {
		return nil, fmt.Errorf("observe label: %w", err)
	}
	obs.LabelCurrent = strings.TrimSpace(labelOut)

	// 4. Check if the last comment is from the bot (approximate heuristic).
	commentOut, err := ghExecWithTimeout(ctx, ghCallTimeout, "issue", "view",
		strconv.Itoa(issueNum),
		"-R", repo,
		"--json", "comments",
		"--jq", ".comments[-1].author.login")
	if err != nil {
		return nil, fmt.Errorf("observe comments: %w", err)
	}
	login := strings.TrimSpace(commentOut)
	// Consider the issue commented if the last commenter is a bot or
	// the workbuddy app. The exact login depends on deployment; a
	// non-empty login from the latest comment is a reasonable v1 signal
	// that the agent posted something.
	obs.IssueCommented = login != "" && (strings.Contains(login, "bot") ||
		strings.Contains(login, "workbuddy") ||
		strings.Contains(login, "[bot]"))

	return obs, nil
}

// isGHNotFound returns true if the error looks like a gh CLI 404 (resource
// not found). gh prints "Not Found" or "HTTP 404" on stderr for missing
// branches/PRs/issues.
func isGHNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Not Found") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "HTTP 404") ||
		strings.Contains(msg, "Could not resolve")
}

// ghExecWithTimeout runs a gh CLI command with a per-call timeout derived
// from the parent context. Returns stdout on success.
func ghExecWithTimeout(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return ghExec(callCtx, args...)
}

// ghExec runs a gh CLI command and returns its stdout. An exit error is
// returned as-is so the caller can distinguish "command failed" from "field
// not found" (the latter produces empty output with exit 0).
func ghExec(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w (stderr: %s)", strings.Join(args[:min(len(args), 3)], " "), err, stderr.String())
	}
	return stdout.String(), nil
}
