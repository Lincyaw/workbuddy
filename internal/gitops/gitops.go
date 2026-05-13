// Package gitops centralizes coordinator-side git and PR operations used by
// the v0.6 coordinator-managed AgentM dispatch path. Other runtimes
// (claude-code, codex) remain self-managed: they invoke `gh issue edit`,
// `git push`, and `gh pr create` from inside the agent subprocess.
//
// The AgentM runtime, by design, does not hold GitHub credentials. After a
// successful run it returns a structured Output containing an artifact
// path (patch/diff). The coordinator (v0.6: still in-process within the
// worker bridge) applies that artifact onto a fresh branch, commits with a
// well-known bot identity, pushes, and opens a PR. See
// docs/decisions/2026-05-13-k8s-agentm-otel.md (Block 2 § Two execution
// modes).
//
// This package shells out to the local `git` and `gh` CLIs rather than
// depending on go-git: workbuddy already requires both binaries on the
// dispatch host, and reusing them keeps the auth/credential surface
// consistent with the rest of the coordinator (reporter, ghadapter).
package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultBotAuthor is the identity coordinator-managed commits are made
// under. Anything that pushes on behalf of an AgentM run uses this unless
// the caller overrides it (e.g. per-repo policy in a future iteration).
var DefaultBotAuthor = Author{
	Name:  "workbuddy-bot",
	Email: "bot@workbuddy.internal",
}

// Author is the git identity attached to a commit. Both fields are
// required; empty values are rejected at CommitAndPush time so we fail
// fast instead of producing a commit with an "unknown" trailer.
type Author struct {
	Name  string
	Email string
}

// CommandRunner is the subprocess boundary used by every shell-out in this
// package. Tests inject a fake runner; production uses the package-level
// ExecRunner that wraps os/exec.
type CommandRunner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) (stdout string, stderr string, err error)
}

// ExecRunner is the default CommandRunner. It invokes the binary on PATH.
type ExecRunner struct{}

// Run implements CommandRunner using os/exec.CommandContext.
func (ExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// Client is the public entry point for callers. The zero value is usable
// and uses ExecRunner + DefaultBotAuthor / `gh` for PR creation.
type Client struct {
	// Runner shells commands out. Defaults to ExecRunner.
	Runner CommandRunner
	// GHBinary names the GitHub CLI binary (defaults to "gh"). Tests
	// inject a fake to assert the arguments passed to `gh pr create`.
	GHBinary string
	// GitBinary names the git CLI binary (defaults to "git").
	GitBinary string
}

func (c *Client) runner() CommandRunner {
	if c != nil && c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c *Client) git() string {
	if c != nil && c.GitBinary != "" {
		return c.GitBinary
	}
	return "git"
}

func (c *Client) gh() string {
	if c != nil && c.GHBinary != "" {
		return c.GHBinary
	}
	return "gh"
}

// CommitAndPush stages every change in repoLocalPath's working tree,
// creates (or resets) the named branch, makes a single commit authored by
// `author` with `message`, and pushes the branch to `origin` (force-with-
// lease so re-dispatch of the same issue overwrites stale remote state
// without losing protected history). repoLocalPath MUST be an existing
// git working tree with a remote named `origin`.
//
// Returns ErrNoChanges if the working tree had no changes to commit:
// AgentM occasionally produces no-op artifacts (e.g. the agent decided the
// task was already satisfied), and the caller should treat that as a
// successful but PR-less run.
func CommitAndPush(ctx context.Context, repoLocalPath, branch, message string, author Author) error {
	c := Client{}
	return c.CommitAndPush(ctx, repoLocalPath, branch, message, author)
}

// CommitAndPush is the method-receiver form of the package-level function.
// Use this when you need to inject a custom Runner (tests, isolation).
func (c *Client) CommitAndPush(ctx context.Context, repoLocalPath, branch, message string, author Author) error {
	if repoLocalPath == "" {
		return errors.New("gitops: empty repoLocalPath")
	}
	if branch == "" {
		return errors.New("gitops: empty branch")
	}
	if message == "" {
		return errors.New("gitops: empty commit message")
	}
	if author.Name == "" || author.Email == "" {
		return errors.New("gitops: author name and email required")
	}
	if info, err := os.Stat(filepath.Join(repoLocalPath, ".git")); err != nil || !(info.IsDir() || info.Mode().IsRegular()) {
		return fmt.Errorf("gitops: %s is not a git working tree", repoLocalPath)
	}

	run := c.runner()
	git := c.git()

	// Create or reset the branch off the current HEAD. We use -B so a
	// previously-leftover branch with the same name (from a re-dispatch)
	// is overwritten rather than producing "branch already exists".
	if _, stderr, err := run.Run(ctx, repoLocalPath, nil, git, "checkout", "-B", branch); err != nil {
		return fmt.Errorf("gitops: git checkout -B %s: %w (%s)", branch, err, strings.TrimSpace(stderr))
	}

	// Stage everything in the worktree (the artifact has already been
	// applied by the caller before this call, or AgentM wrote files
	// directly into the workspace).
	if _, stderr, err := run.Run(ctx, repoLocalPath, nil, git, "add", "-A"); err != nil {
		return fmt.Errorf("gitops: git add: %w (%s)", err, strings.TrimSpace(stderr))
	}

	// If there are no staged changes, return ErrNoChanges. Distinguishing
	// this from a real failure matters: we don't want to push an empty
	// branch or open a PR with no diff.
	if stdout, _, err := run.Run(ctx, repoLocalPath, nil, git, "status", "--porcelain"); err == nil {
		if strings.TrimSpace(stdout) == "" {
			return ErrNoChanges
		}
	}

	env := []string{
		"GIT_AUTHOR_NAME=" + author.Name,
		"GIT_AUTHOR_EMAIL=" + author.Email,
		"GIT_COMMITTER_NAME=" + author.Name,
		"GIT_COMMITTER_EMAIL=" + author.Email,
	}
	if _, stderr, err := run.Run(ctx, repoLocalPath, env, git, "commit", "-m", message); err != nil {
		// `nothing to commit` can still slip through if status reported
		// untracked-but-ignored entries; treat it the same as the
		// status check above.
		if strings.Contains(stderr, "nothing to commit") {
			return ErrNoChanges
		}
		return fmt.Errorf("gitops: git commit: %w (%s)", err, strings.TrimSpace(stderr))
	}

	if _, stderr, err := run.Run(ctx, repoLocalPath, nil, git, "push", "--force-with-lease", "origin", branch); err != nil {
		return fmt.Errorf("gitops: git push: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return nil
}

// ErrNoChanges is returned by CommitAndPush when the working tree had no
// changes to commit after `git add -A`. The caller MUST NOT call OpenPR
// in this case.
var ErrNoChanges = errors.New("gitops: no changes to commit")

// OpenPR creates a pull request on `repo` (owner/name form) from
// `branch` against the repo's default base branch, with the given title
// and body. Returns the PR URL emitted by `gh pr create` on success.
//
// `gh` already handles authentication (it reads GH_TOKEN/GITHUB_TOKEN or
// the gh keyring), and `gh pr create` works against both github.com and
// Gitea installations the user has logged into.
func OpenPR(ctx context.Context, repo, branch, title, body string) (string, error) {
	c := Client{}
	return c.OpenPR(ctx, repo, branch, title, body)
}

// OpenPR is the method-receiver form of the package-level function.
func (c *Client) OpenPR(ctx context.Context, repo, branch, title, body string) (string, error) {
	if repo == "" {
		return "", errors.New("gitops: empty repo")
	}
	if branch == "" {
		return "", errors.New("gitops: empty branch")
	}
	if title == "" {
		return "", errors.New("gitops: empty PR title")
	}

	args := []string{
		"pr", "create",
		"--repo", repo,
		"--head", branch,
		"--title", title,
		"--body", body,
	}
	stdout, stderr, err := c.runner().Run(ctx, "", nil, c.gh(), args...)
	if err != nil {
		// gh prints the URL on success; on failure stderr carries the
		// reason. Surface it so the reporter can render the issue
		// comment.
		return "", fmt.Errorf("gitops: gh pr create: %w (%s)", err, strings.TrimSpace(stderr))
	}
	// gh emits the PR URL as the last non-empty line of stdout.
	url := lastNonEmptyLine(stdout)
	if url == "" {
		return "", fmt.Errorf("gitops: gh pr create returned no URL (stdout=%q)", stdout)
	}
	return url, nil
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}
