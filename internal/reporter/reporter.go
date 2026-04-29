package reporter

import (
	"context"
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
	"github.com/Lincyaw/workbuddy/internal/ghadapter"
	"github.com/Lincyaw/workbuddy/internal/ghutil"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/sessionref"
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

// GHCLIWriter implements GHCommentWriter using the shared gh CLI adapter.
type GHCLIWriter struct {
	Client *ghadapter.CLI
}

func (g *GHCLIWriter) client() *ghadapter.CLI {
	if g != nil && g.Client != nil {
		return g.Client
	}
	return ghadapter.NewCLI()
}

// WriteComment posts a comment to the given issue via gh issue comment.
func (g *GHCLIWriter) WriteComment(repo string, issueNum int, body string) error {
	return g.client().WriteIssueComment(context.Background(), repo, issueNum, body)
}

// ReactionManager abstracts adding/removing emoji reactions on issues so the
// reporter can be tested without invoking gh. The default implementation
// (GHCLIReactionManager) shells out to `gh api`.
type ReactionManager interface {
	SetBlockedReaction(ctx context.Context, repo string, issueNum int, blocked bool) error
}

// GHCLIReactionManager implements ReactionManager via the shared gh CLI adapter.
type GHCLIReactionManager struct {
	Client *ghadapter.CLI
	// botLoginOnce / botLogin caches the authenticated login so we only resolve it once.
	botLoginOnce sync.Once
	botLogin     string
	botLoginErr  error
}

func (g *GHCLIReactionManager) client() *ghadapter.CLI {
	if g != nil && g.Client != nil {
		return g.Client
	}
	return ghadapter.NewCLI()
}

func (g *GHCLIReactionManager) authenticatedLogin(ctx context.Context) (string, error) {
	g.botLoginOnce.Do(func() {
		login, err := g.client().AuthenticatedLogin(ctx)
		if err != nil {
			g.botLoginErr = err
			return
		}
		g.botLogin = strings.TrimSpace(login)
	})
	return g.botLogin, g.botLoginErr
}

// SetBlockedReaction adds or removes the bot's own reaction on the issue.
func (g *GHCLIReactionManager) SetBlockedReaction(ctx context.Context, repo string, issueNum int, blocked bool) error {
	if blocked {
		return g.client().AddIssueReaction(ctx, repo, issueNum, ReactionConfused)
	}

	login, err := g.authenticatedLogin(ctx)
	if err != nil {
		return err
	}

	reactions, err := g.client().ListIssueReactions(ctx, repo, issueNum)
	if err != nil {
		return err
	}
	for _, reaction := range reactions {
		if reaction.Content != ReactionConfused {
			continue
		}
		if login != "" && reaction.User != login {
			continue
		}
		if err := g.client().DeleteIssueReaction(ctx, repo, issueNum, reaction.ID); err != nil {
			return err
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
	gh             GHCommentWriter
	reactions      ReactionManager
	baseURL        string // e.g. "http://localhost:8080", empty to omit session links
	eventlog       EventRecorder
	verifier       ClaimVerifier
	now            func() time.Time
	cycleCapLoader CycleCapTrailLoader
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
	return &Reporter{gh: gh, reactions: &GHCLIReactionManager{}, now: time.Now}
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

func (r *Reporter) sessionURL(sessionID, workerID string) string {
	return sessionref.BuildURL(r.baseURL, workerID, sessionID)
}

// SetVerifier sets the claim verifier used to check agent side-effects.
func (r *Reporter) SetVerifier(v ClaimVerifier) {
	r.verifier = v
}

// Verify runs claim verification against the agent result.
// It returns nil when the verifier is not configured or the run did not succeed.
func (r *Reporter) Verify(ctx context.Context, repo string, issueNum int, result *runtimepkg.Result) (*VerificationResult, error) {
	if r.verifier == nil || result == nil || result.ExitCode != 0 {
		return nil, nil
	}
	finishedAt := r.now()
	return r.verifier.Verify(ctx, repo, issueNum, VerificationInput{
		Output:    reportOutput(result),
		StartedAt: finishedAt.Add(-result.Duration),
		EndedAt:   finishedAt,
	})
}

// ReportStarted posts an "Agent Started" comment with a session link before execution begins.
func (r *Reporter) ReportStarted(ctx context.Context, repo string, issueNum int, agentName, sessionID, workerID string) error {
	body := FormatStartedReport(StartedData{
		AgentName:  agentName,
		SessionID:  sessionID,
		WorkerID:   workerID,
		SessionURL: r.sessionURL(sessionID, workerID),
		StartedAt:  time.Now(),
	})
	return r.writeWithRateLimitRetry(ctx, repo, issueNum, "report_started", func() error {
		return r.gh.WriteComment(repo, issueNum, body)
	})
}

// ReportNeedsHuman posts a needs-human recommendation comment for a transition.
func (r *Reporter) ReportNeedsHuman(ctx context.Context, repo string, issueNum int, labelLine string) error {
	body := FormatNeedsHumanReport(NeedsHumanData{
		LabelLine: labelLine,
		Timestamp: time.Now(),
	})
	return r.writeWithRateLimitRetry(ctx, repo, issueNum, "needs_human", func() error {
		return r.gh.WriteComment(repo, issueNum, body)
	})
}

// CycleCapTrailLoader fetches the rejection-trail digest entries that are
// rendered into the dev↔review cycle-cap needs-human comment. The Reporter
// calls this when posting a cap-hit comment so digest assembly stays in
// Coordinator Go code (no agent re-invocation, AC-3).
type CycleCapTrailLoader interface {
	LoadCycleCapTrail(ctx context.Context, repo string, issueNum int) ([]CycleRejectionEntry, string, string, error)
}

// SetCycleCapTrailLoader installs the loader used by ReportDevReviewCycleCap
// to assemble the rejection-trail digest. When unset the comment is still
// posted but with an empty trail.
func (r *Reporter) SetCycleCapTrailLoader(l CycleCapTrailLoader) {
	r.cycleCapLoader = l
}

// ReportDevReviewCycleCap posts the needs-human comment when an issue has
// tripped the dev↔review cycle cap. The comment includes a rejection-trail
// digest built from existing event metadata (no agent re-invocation, AC-3).
//
// info is the StateMachine's CycleCapInfo expressed locally so this package
// does not depend on internal/statemachine. Callers in production wiring
// translate at the boundary.
func (r *Reporter) ReportDevReviewCycleCap(ctx context.Context, repo string, issueNum int, workflowName string, cycleCount, maxReviewCycles int, hitAt time.Time) error {
	d := CycleCapData{
		WorkflowName:    workflowName,
		CycleCount:      cycleCount,
		MaxReviewCycles: maxReviewCycles,
		HitAt:           hitAt,
	}
	if r.cycleCapLoader != nil {
		trail, prURL, branch, err := r.cycleCapLoader.LoadCycleCapTrail(ctx, repo, issueNum)
		if err != nil {
			// Non-fatal: post the comment with an empty trail rather than
			// failing the cap notification because the digest source is
			// unavailable.
			fmt.Fprintf(os.Stderr, "reporter: load cycle-cap trail for %s#%d: %v\n", repo, issueNum, err)
		} else {
			d.RejectionTrail = trail
			d.LatestPRURL = prURL
			d.BranchName = branch
		}
	}
	body := FormatCycleCapReport(d)
	return r.writeWithRateLimitRetry(ctx, repo, issueNum, "dev_review_cycle_cap", func() error {
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
	result *runtimepkg.Result,
	sessionID, workerID string,
	retryCount, maxRetries int,
	labelLine string,
	workDir string,
	syncFailure *SyncFailure,
) error {
	_, err := r.ReportWithVerification(ctx, repo, issueNum, agentName, result, sessionID, workerID, retryCount, maxRetries, labelLine, workDir, syncFailure)
	return err
}

// ReportVerified formats and posts an agent execution report using a
// precomputed verification result. This avoids running the same external
// verification commands twice when the caller already needed the result for
// control-flow decisions.
func (r *Reporter) ReportVerified(
	ctx context.Context,
	repo string,
	issueNum int,
	agentName string,
	result *runtimepkg.Result,
	sessionID, workerID string,
	retryCount, maxRetries int,
	labelLine string,
	workDir string,
	verification *VerificationResult,
	syncFailure *SyncFailure,
) error {
	_, err := r.report(ctx, repo, issueNum, agentName, result, sessionID, workerID, retryCount, maxRetries, labelLine, workDir, verification, syncFailure)
	return err
}

// ReportWithVerification is like Report but runs claim verification for
// successful exits and returns the verification result. Callers should treat
// a non-nil VerificationResult with Partial == true as a failed run for
// state-machine purposes.
func (r *Reporter) ReportWithVerification(
	ctx context.Context,
	repo string,
	issueNum int,
	agentName string,
	result *runtimepkg.Result,
	sessionID, workerID string,
	retryCount, maxRetries int,
	labelLine string,
	workDir string,
	syncFailure *SyncFailure,
) (*VerificationResult, error) {
	return r.report(ctx, repo, issueNum, agentName, result, sessionID, workerID, retryCount, maxRetries, labelLine, workDir, nil, syncFailure)
}

func (r *Reporter) report(
	ctx context.Context,
	repo string,
	issueNum int,
	agentName string,
	result *runtimepkg.Result,
	sessionID, workerID string,
	retryCount, maxRetries int,
	labelLine string,
	workDir string,
	verification *VerificationResult,
	syncFailure *SyncFailure,
) (*VerificationResult, error) {
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

	// Infra failure takes precedence over "failure" / "timeout" verdicts —
	// the launcher could not deliver a verdict the coordinator can trust, so
	// we render this with a distinct header and explicitly disclaim the
	// agent-verdict interpretation. See issue #131.
	var infraReason string
	if runtimepkg.IsInfraFailure(result) {
		status = "infra-error"
		if result.Meta != nil {
			infraReason = result.Meta[runtimepkg.MetaInfraFailureReason]
		}
		if result.Stderr != "" {
			errorDetail = result.Stderr
		}
	}

	// Run claim verification for successful runs
	if status == "success" && verification == nil && r.verifier != nil {
		finishedAt := r.now()
		vRes, vErr := r.verifier.Verify(ctx, repo, issueNum, VerificationInput{
			Output:    reportOutput(result),
			StartedAt: finishedAt.Add(-result.Duration),
			EndedAt:   finishedAt,
		})
		if vErr != nil {
			if r.eventlog != nil {
				r.eventlog.Log(eventlog.TypeError, repo, issueNum, map[string]any{
					"source": "claim_verification",
					"error":  vErr.Error(),
				})
			}
		} else {
			verification = vRes
			if verification != nil && verification.Partial {
				status = "partial"
				if len(verification.Checks) > 0 {
					var sb strings.Builder
					sb.WriteString("Claim verification failed:\n")
					for _, c := range verification.Checks {
						if c.OK {
							continue
						}
						sb.WriteString(fmt.Sprintf("- %s: claimed %q, actual %q\n", c.Type, c.Claim, c.Actual))
					}
					errorDetail = sb.String()
				}
			}
		}
	}

	// Extract PR link from meta or stdout
	prLink := ""
	if result.Meta != nil {
		prLink = result.Meta["pr_url"]
	}

	output := reportOutput(result)
	if output == "" && result.Stderr != "" {
		output = result.Stderr
	}

	data := ReportData{
		AgentName:    agentName,
		Status:       status,
		Duration:     result.Duration,
		SessionID:    sessionID,
		WorkerID:     workerID,
		RetryCount:   retryCount,
		MaxRetries:   maxRetries,
		Output:       output,
		PRLink:       prLink,
		ErrorDetail:  errorDetail,
		SessionURL:   r.sessionURL(sessionID, workerID),
		LabelLine:    labelLine,
		Verification: verification,
		InfraReason:  infraReason,
		SyncFailure:  syncFailure,
	}

	body := FormatReportAt(data, time.Now())
	if bodySizeBytes(body) > maxCommentBodyBytes {
		return verification, r.reportWithOverflow(ctx, repo, issueNum, agentName, body, workDir)
	}
	return verification, r.writeWithRateLimitRetry(ctx, repo, issueNum, "report", func() error {
		return r.gh.WriteComment(repo, issueNum, body)
	})
}

func reportOutput(result *runtimepkg.Result) string {
	if result == nil {
		return ""
	}
	return strings.TrimSpace(result.LastMessage)
}

func (r *Reporter) reportWithOverflow(ctx context.Context, repo string, issueNum int, agentName, body, workDir string) error {
	artifactURL := ""
	var commitErr error
	if workDir != "" {
		artifactURL, commitErr = r.commitOverflowArtifact(ctx, workDir, repo, issueNum, agentName, body)
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
