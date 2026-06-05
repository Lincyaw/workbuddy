package control

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// GHObserver implements Observer via the gh CLI.
// It runs on the workbuddy pod and uses the pod's GH_TOKEN.
type GHObserver struct{}

var _ Observer = (*GHObserver)(nil)

func (o *GHObserver) Observe(ctx context.Context, repo string, issueNum int, branch string) (*Observation, error) {
	obs := &Observation{}

	// 1. Check if branch exists on remote.
	branchOut, err := ghExec(ctx, "api", fmt.Sprintf("repos/%s/branches/%s", repo, branch),
		"--silent", "--jq", ".name")
	if err == nil && strings.TrimSpace(branchOut) != "" {
		obs.BranchPushed = true
	}
	// A 404 is not an error — branch simply does not exist yet.

	// 2. Check for open PR targeting this branch.
	prOut, err := ghExec(ctx, "pr", "list",
		"-R", repo,
		"--head", branch,
		"--state", "open",
		"--json", "number",
		"--jq", ".[0].number")
	if err == nil {
		if n, perr := strconv.Atoi(strings.TrimSpace(prOut)); perr == nil && n > 0 {
			obs.PROpen = true
			obs.PRNumber = n
		}
	}

	// 3. Get current status:* label on the issue.
	labelOut, err := ghExec(ctx, "issue", "view",
		strconv.Itoa(issueNum),
		"-R", repo,
		"--json", "labels",
		"--jq", `[.labels[].name] | map(select(startswith("status:")))[0]`)
	if err == nil {
		obs.LabelCurrent = strings.TrimSpace(labelOut)
	}

	// 4. Check if the last comment is from the bot (approximate heuristic).
	commentOut, err := ghExec(ctx, "issue", "view",
		strconv.Itoa(issueNum),
		"-R", repo,
		"--json", "comments",
		"--jq", ".comments[-1].author.login")
	if err == nil {
		login := strings.TrimSpace(commentOut)
		// Consider the issue commented if the last commenter is a bot or
		// the workbuddy app. The exact login depends on deployment; a
		// non-empty login from the latest comment is a reasonable v1 signal
		// that the agent posted something.
		obs.IssueCommented = login != "" && (strings.Contains(login, "bot") ||
			strings.Contains(login, "workbuddy") ||
			strings.Contains(login, "[bot]"))
	}

	return obs, nil
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
