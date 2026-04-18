package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/ghutil"
	"github.com/Lincyaw/workbuddy/internal/launcher"
)

// ReactionConfused is the GitHub reaction content used to signal that an
// issue is currently dependency-blocked. (gh API content vocabulary:
// +1, -1, laugh, confused, heart, hooray, rocket, eyes.)
const ReactionConfused = "confused"

// overflowReportsDir is a tracked path in the repo where oversized reports
// are committed. We intentionally avoid .workbuddy/ because that directory is
// gitignored for local runtime state.
const overflowReportsDir = "scripts/review-reports"

// maxCommentBodyBytes is the maximum encoded body size the reporter will post.
// We enforce the guard in bytes because gh transmits UTF-8 text and we want a
// conservative preflight check with headroom below GitHub's hard limit.
const maxCommentBodyBytes = 60000

// GHCommentWriter abstracts the gh issue comment command for testing.
type GHCommentWriter interface {
	WriteComment(repo string, issueNum int, body string) error
}

// GHCLIWriter implements GHCommentWriter using the gh CLI.
type GHCLIWriter struct{}

// WriteComment posts a comment to the given issue via gh issue comment.
// The body is passed via stdin using --body-file - to avoid "argument list too long".
func (g *GHCLIWriter) WriteComment(repo string, issueNum int, body string) error {
	cmd := exec.Command("gh", "issue", "comment",
		fmt.Sprintf("%d", issueNum),
		"--repo", repo,
		"--body-file", "-",
	)
	cmd.Stdin = strings.NewReader(body)
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

// SetBlockedReaction adds or removes the bot's own reaction on the issue.
//
// blocked=true → POST a confused reaction (idempotent on GitHub side: if the
// authenticated user already reacted with `confused`, GitHub returns 200
// without creating a duplicate).
//
// blocked=false → fetch all reactions on the issue, filter for `content ==
// confused` authored by the bot's own login, and DELETE each one.
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

// EventRecorder captures lightweight event records.
type EventRecorder interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

// Reporter writes execution reports as GitHub Issue comments.
type Reporter struct {
	gh        GHCommentWriter
	reactions ReactionManager
	baseURL   string // e.g. "http://localhost:8080", empty to omit session links
	eventlog  EventRecorder
}

var (
	// rateLimitRetryDelays is overridden in tests to keep assertions fast.
	rateLimitRetryDelays = []time.Duration{
		30 * time.Second,
		60 * time.Second,
		90 * time.Second,
	}
	// rateLimitRetryLimit is the maximum number of retries after the first attempt.
	rateLimitRetryLimit = 3
)

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

// SetEventRecorder sets the optional EventRecorder.
func (r *Reporter) SetEventRecorder(logger EventRecorder) {
	r.eventlog = logger
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
func (r *Reporter) ReportStarted(ctx context.Context, repo string, issueNum int, agentName, sessionID, workerID string) error {
	var sessionURL string
	if r.baseURL != "" {
		sessionURL = r.baseURL + "/sessions/" + sessionID
	}
	body := FormatStartedReport(agentName, sessionID, workerID, sessionURL, time.Now())
	return r.writeWithRateLimitRetry(ctx, repo, issueNum, "report_started", func() error {
		return r.gh.WriteComment(repo, issueNum, body)
	})
}

// ReportNeedsHuman posts a needs-human recommendation comment for a transition.
func (r *Reporter) ReportNeedsHuman(ctx context.Context, repo string, issueNum int, labelLine string) error {
	body := FormatNeedsHumanReport(labelLine, time.Now())
	return r.writeWithRateLimitRetry(ctx, repo, issueNum, "needs_human", func() error {
		return r.gh.WriteComment(repo, issueNum, body)
	})
}

// Report formats and posts an agent execution report to the issue.
// If the formatted body exceeds maxCommentBodyBytes, the overflow is written
// to a file in workDir, committed, pushed, and a short summary is posted
// instead.
func (r *Reporter) Report(
	ctx context.Context,
	repo string,
	issueNum int,
	agentName string,
	result *launcher.Result,
	sessionID, workerID string,
	retryCount, maxRetries int,
	labelLine string,
	workDir string,
) error {
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
	if bodySizeBytes(body) > maxCommentBodyBytes {
		return r.reportWithOverflow(ctx, repo, issueNum, agentName, body, workDir)
	}
	return r.writeWithRateLimitRetry(ctx, repo, issueNum, "report", func() error {
		return r.gh.WriteComment(repo, issueNum, body)
	})
}

func (r *Reporter) reportWithOverflow(ctx context.Context, repo string, issueNum int, agentName, body, workDir string) error {
	artifactURL := ""
	var commitErr error
	if workDir != "" {
		artifactURL, commitErr = r.commitOverflowArtifact(ctx, workDir, repo, issueNum, agentName, body)
	} else {
		commitErr = fmt.Errorf("workDir is empty")
	}

	if r.eventlog != nil {
		payload := map[string]any{
			"source":       "report",
			"body_bytes":   bodySizeBytes(body),
			"committed":    commitErr == nil && artifactURL != "",
			"artifact_url": artifactURL,
		}
		if commitErr != nil {
			payload["commit_error"] = commitErr.Error()
		}
		r.eventlog.Log(eventlog.TypeReportOverflow, repo, issueNum, payload)
	}

	shortBody := truncateReport(body, artifactURL)
	return r.writeWithRateLimitRetry(ctx, repo, issueNum, "report", func() error {
		return r.gh.WriteComment(repo, issueNum, shortBody)
	})
}

func (r *Reporter) commitOverflowArtifact(ctx context.Context, workDir, repo string, issueNum int, agentName, body string) (string, error) {
	gitDir := filepath.Join(workDir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}

	branch, err := r.gitBranch(ctx, workDir)
	if err != nil {
		return "", fmt.Errorf("determine branch: %w", err)
	}

	reportsDir := filepath.Join(workDir, overflowReportsDir)
	if err := os.MkdirAll(reportsDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}

	filename := fmt.Sprintf("issue-%d-%s-%d.md", issueNum, sanitizeArtifactComponent(agentName), time.Now().Unix())
	filePath := filepath.Join(reportsDir, filename)
	if err := os.WriteFile(filePath, []byte(body), 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	if _, err := r.execGit(ctx, workDir, "add", filePath); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}

	env := []string{
		"GIT_AUTHOR_NAME=workbuddy",
		"GIT_AUTHOR_EMAIL=workbuddy@localhost",
		"GIT_COMMITTER_NAME=workbuddy",
		"GIT_COMMITTER_EMAIL=workbuddy@localhost",
	}
	_, commitErr := r.execGitEnv(ctx, workDir, env, "commit", "-m", fmt.Sprintf("workbuddy: overflow report for issue #%d", issueNum))
	if commitErr != nil && !strings.Contains(commitErr.Error(), "nothing to commit") {
		return "", fmt.Errorf("git commit: %w", commitErr)
	}

	if _, err := r.execGit(ctx, workDir, "push", "origin", branch); err != nil {
		return "", fmt.Errorf("git push: %w", err)
	}

	relPath := filepath.Join(overflowReportsDir, filename)
	relPath = strings.ReplaceAll(relPath, string(os.PathSeparator), "/")
	return fmt.Sprintf("https://github.com/%s/blob/%s/%s", repo, branch, relPath), nil
}

func (r *Reporter) gitBranch(ctx context.Context, workDir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", workDir, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("empty branch name")
	}
	return branch, nil
}

func (r *Reporter) execGit(ctx context.Context, workDir string, args ...string) (string, error) {
	return r.execGitEnv(ctx, workDir, nil, args...)
}

func (r *Reporter) execGitEnv(ctx context.Context, workDir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workDir}, args...)...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

func truncateReport(body, artifactURL string) string {
	short := utf8Prefix(body, 2000)
	if idx := strings.LastIndex(short, "\n"); idx > 0 {
		short = short[:idx]
	}

	var b strings.Builder
	b.WriteString(short)
	b.WriteString("\n\n")
	b.WriteString("> :warning: **Report truncated due to size limit.**\n\n")
	if artifactURL != "" {
		fmt.Fprintf(&b, "[View full report](%s)\n\n", artifactURL)
	} else {
		b.WriteString("*(Full report could not be committed to branch.)*\n\n")
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "*workbuddy coordinator | %s*", time.Now().UTC().Format(time.RFC3339))
	return b.String()
}

func sanitizeArtifactComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "agent"
	}

	var b strings.Builder
	b.Grow(len(value))
	lastDash := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			lastDash = false
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}

	sanitized := strings.Trim(b.String(), "-")
	if sanitized == "" {
		return "agent"
	}
	return sanitized
}

func bodySizeBytes(value string) int {
	return len(value)
}

func utf8Prefix(value string, maxBytes int) string {
	if maxBytes <= 0 || value == "" {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut]
}

func (r *Reporter) writeWithRateLimitRetry(ctx context.Context, repo string, issueNum int, source string, writeFn func() error) error {
	if writeFn == nil {
		return nil
	}
	for attempt := 0; attempt <= rateLimitRetryLimit; attempt++ {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		err := writeFn()
		if err == nil {
			if r.eventlog != nil {
				r.eventlog.Log(eventlog.TypeReport, repo, issueNum, map[string]any{
					"source": source,
					"status": "success",
				})
			}
			return nil
		}
		if !ghutil.IsRateLimit(err) {
			return err
		}
		r.logRateLimit(source, repo, issueNum, err)
		if attempt >= rateLimitRetryLimit {
			return err
		}
		delayIdx := attempt
		if len(rateLimitRetryDelays) == 0 {
			continue
		}
		if delayIdx >= len(rateLimitRetryDelays) {
			delayIdx = len(rateLimitRetryDelays) - 1
		}
		delay := rateLimitRetryDelays[delayIdx]
		if delay > 0 {
			timer := time.NewTimer(delay)
			if ctx == nil {
				<-timer.C
			} else {
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
	}
	return nil
}

func (r *Reporter) logRateLimit(source, repo string, issueNum int, err error) {
	if r.eventlog == nil || err == nil {
		return
	}
	r.eventlog.Log(eventlog.TypeRateLimit, repo, issueNum, map[string]any{
		"source": source,
		"error":  ghutil.RedactTokens(err.Error()),
	})
}
