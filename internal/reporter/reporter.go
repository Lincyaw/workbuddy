package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/launcher"
)

// ReactionConfused is the GitHub reaction content used to signal that an
// issue is currently dependency-blocked. (gh API content vocabulary:
// +1, -1, laugh, confused, heart, hooray, rocket, eyes.)
const ReactionConfused = "confused"

// GHCommentWriter abstracts the gh issue comment command for testing.
type GHCommentWriter interface {
	WriteComment(repo string, issueNum int, body string) error
}

// GHCLIWriter implements GHCommentWriter using the gh CLI.
type GHCLIWriter struct{}

// WriteComment posts a comment to the given issue via gh issue comment.
func (g *GHCLIWriter) WriteComment(repo string, issueNum int, body string) error {
	cmd := exec.Command("gh", "issue", "comment",
		fmt.Sprintf("%d", issueNum),
		"--repo", repo,
		"--body", body,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("reporter: gh issue comment: %s: %w", string(output), err)
	}
	return nil
}

// ReactionManager abstracts adding/removing emoji reactions on issues so the
// reporter can be tested without invoking gh. The default implementation
// (GHCLIReactionManager) shells out to `gh api`.
type ReactionManager interface {
	SetBlockedReaction(ctx context.Context, repo string, issueNum int, blocked bool) error
}

// GHCLIReactionManager implements ReactionManager via the gh CLI.
type GHCLIReactionManager struct {
	// botLoginOnce / botLogin caches `gh api user --jq .login` so we only
	// shell out once per process to identify the bot's own reactions.
	botLoginOnce sync.Once
	botLogin     string
	botLoginErr  error
}

func (g *GHCLIReactionManager) authenticatedLogin(ctx context.Context) (string, error) {
	g.botLoginOnce.Do(func() {
		cmd := exec.CommandContext(ctx, "gh", "api", "user", "--jq", ".login")
		out, err := cmd.Output()
		if err != nil {
			g.botLoginErr = fmt.Errorf("reporter: gh api user: %w", err)
			return
		}
		g.botLogin = strings.TrimSpace(string(out))
	})
	return g.botLogin, g.botLoginErr
}

// SetBlockedReaction adds or removes the bot's own 😕 reaction on the issue.
//
// blocked=true → POST a confused reaction (idempotent on GitHub side: if the
// authenticated user already reacted with `confused`, GitHub returns 200
// without creating a duplicate).
//
// blocked=false → fetch all reactions on the issue, filter for `content ==
// "confused"` authored by the bot's own login, and DELETE each one.
func (g *GHCLIReactionManager) SetBlockedReaction(ctx context.Context, repo string, issueNum int, blocked bool) error {
	endpoint := fmt.Sprintf("repos/%s/issues/%d/reactions", repo, issueNum)
	if blocked {
		cmd := exec.CommandContext(ctx, "gh", "api", "-X", "POST", endpoint,
			"-f", "content="+ReactionConfused,
			"-H", "Accept: application/vnd.github+json",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("reporter: gh api POST reactions: %s: %w", string(out), err)
		}
		return nil
	}

	login, err := g.authenticatedLogin(ctx)
	if err != nil {
		return err
	}

	listCmd := exec.CommandContext(ctx, "gh", "api", endpoint,
		"-H", "Accept: application/vnd.github+json",
	)
	out, err := listCmd.Output()
	if err != nil {
		return fmt.Errorf("reporter: gh api GET reactions: %w", err)
	}
	var reactions []struct {
		ID      int64  `json:"id"`
		Content string `json:"content"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(out, &reactions); err != nil {
		return fmt.Errorf("reporter: parse reactions: %w", err)
	}
	for _, r := range reactions {
		if r.Content != ReactionConfused {
			continue
		}
		if login != "" && r.User.Login != login {
			continue
		}
		delEndpoint := fmt.Sprintf("repos/%s/issues/%d/reactions/%d", repo, issueNum, r.ID)
		delCmd := exec.CommandContext(ctx, "gh", "api", "-X", "DELETE", delEndpoint)
		if delOut, err := delCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("reporter: gh api DELETE reactions/%d: %s: %w", r.ID, string(delOut), err)
		}
	}
	return nil
}

// Reporter writes execution reports as GitHub Issue comments.
type Reporter struct {
	gh        GHCommentWriter
	reactions ReactionManager
	baseURL   string // e.g. "http://localhost:8080", empty to omit session links
}

// NewReporter creates a Reporter with the given GH comment writer. The
// reaction manager defaults to a GHCLIReactionManager; callers may override
// it via SetReactionManager (e.g., tests).
func NewReporter(gh GHCommentWriter) *Reporter {
	return &Reporter{gh: gh, reactions: &GHCLIReactionManager{}}
}

// SetReactionManager replaces the default ReactionManager (used by tests).
func (r *Reporter) SetReactionManager(m ReactionManager) {
	r.reactions = m
}

// SetBlockedReaction is a thin pass-through to the configured ReactionManager.
// Used by the Coordinator's per-cycle dependency reaction reconciler.
func (r *Reporter) SetBlockedReaction(ctx context.Context, repo string, issueNum int, blocked bool) error {
	if r.reactions == nil {
		return nil
	}
	return r.reactions.SetBlockedReaction(ctx, repo, issueNum, blocked)
}

// SetBaseURL sets the base URL for session detail links in reports.
func (r *Reporter) SetBaseURL(baseURL string) {
	r.baseURL = baseURL
}

// ReportStarted posts an "Agent Started" comment with a session link before execution begins.
func (r *Reporter) ReportStarted(repo string, issueNum int, agentName, sessionID, workerID string) error {
	var sessionURL string
	if r.baseURL != "" {
		sessionURL = r.baseURL + "/sessions/" + sessionID
	}
	body := FormatStartedReport(agentName, sessionID, workerID, sessionURL, time.Now())
	return r.gh.WriteComment(repo, issueNum, body)
}

func (r *Reporter) ReportNeedsHuman(repo string, issueNum int, labelLine string) error {
	body := FormatNeedsHumanReport(labelLine, time.Now())
	return r.gh.WriteComment(repo, issueNum, body)
}

// Report formats and posts an agent execution report to the issue.
func (r *Reporter) Report(repo string, issueNum int, agentName string, result *launcher.Result,
	sessionID, workerID string, retryCount, maxRetries int, labelLine string) error {

	status := "success"
	var errorDetail string
	if result.ExitCode != 0 {
		status = "failure"
		if result.Stderr != "" {
			errorDetail = result.Stderr
		}
	}

	// Check for timeout via meta
	if result.Meta != nil {
		if result.Meta["timeout"] == "true" {
			status = "timeout"
		}
		if result.Meta["retry_limit"] == "true" {
			status = "retry-limit"
		}
	}

	// Extract PR link from meta or stdout
	prLink := ""
	if result.Meta != nil {
		prLink = result.Meta["pr_url"]
	}

	output := result.LastMessage
	if output == "" {
		output = result.Stdout
	}
	if output == "" && result.Stderr != "" {
		output = result.Stderr
	}

	var sessionURL string
	if r.baseURL != "" {
		sessionURL = r.baseURL + "/sessions/" + sessionID
	}

	data := ReportData{
		AgentName:   agentName,
		Status:      status,
		Duration:    result.Duration,
		SessionID:   sessionID,
		WorkerID:    workerID,
		RetryCount:  retryCount,
		MaxRetries:  maxRetries,
		Output:      output,
		PRLink:      prLink,
		ErrorDetail: errorDetail,
		SessionURL:  sessionURL,
		LabelLine:   labelLine,
	}

	body := FormatReportAt(data, time.Now())
	return r.gh.WriteComment(repo, issueNum, body)
}
