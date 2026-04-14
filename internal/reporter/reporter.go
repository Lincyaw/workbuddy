package reporter

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/launcher"
)

// GHWriter abstracts GitHub write operations for the reporter.
type GHWriter interface {
	CommentOnIssue(repo string, issueNum int, body string) error
}

// EventRecorder abstracts event logging so tests can use a fake.
type EventRecorder interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

// ReportInput contains all information needed to generate an execution report.
type ReportInput struct {
	Repo       string
	IssueNum   int
	AgentName  string
	WorkerID   string
	SessionID  string
	Result     *launcher.Result
	RetryCount int
	MaxRetries int
	Status     string // "success", "failure", "timeout"
}

// Reporter writes execution summaries to GitHub Issues as comments.
type Reporter struct {
	gh       GHWriter
	eventlog EventRecorder
}

// NewReporter creates a Reporter.
func NewReporter(gh GHWriter, el EventRecorder) *Reporter {
	return &Reporter{
		gh:       gh,
		eventlog: el,
	}
}

// Report generates and posts an execution summary comment on the issue.
func (r *Reporter) Report(input ReportInput) error {
	body := FormatReport(input)

	// Try to post the comment. On failure, retry once.
	err := r.gh.CommentOnIssue(input.Repo, input.IssueNum, body)
	if err != nil {
		log.Printf("[reporter] first attempt to comment on %s#%d failed: %v, retrying...", input.Repo, input.IssueNum, err)
		err = r.gh.CommentOnIssue(input.Repo, input.IssueNum, body)
		if err != nil {
			log.Printf("[reporter] retry failed for %s#%d: %v", input.Repo, input.IssueNum, err)
			r.eventlog.Log("error", input.Repo, input.IssueNum,
				fmt.Sprintf(`{"error":"reporter: gh comment failed after retry","detail":"%s"}`, err.Error()))
			return fmt.Errorf("reporter: comment on issue: %w", err)
		}
	}

	r.eventlog.Log("report", input.Repo, input.IssueNum,
		fmt.Sprintf(`{"agent":"%s","status":"%s","session":"%s"}`, input.AgentName, input.Status, input.SessionID))
	return nil
}

// FormatReport generates the Markdown body for an execution report.
func FormatReport(input ReportInput) string {
	var b strings.Builder

	// Status emoji/indicator.
	statusIcon := statusToIcon(input.Status)

	b.WriteString(fmt.Sprintf("## %s Agent Execution Report\n\n", statusIcon))

	// Summary table.
	b.WriteString("| Field | Value |\n")
	b.WriteString("|-------|-------|\n")
	b.WriteString(fmt.Sprintf("| Agent | `%s` |\n", input.AgentName))
	b.WriteString(fmt.Sprintf("| Status | %s %s |\n", statusIcon, input.Status))
	if input.Result != nil {
		b.WriteString(fmt.Sprintf("| Duration | %s |\n", formatDuration(input.Result.Duration)))
	}
	b.WriteString(fmt.Sprintf("| Session ID | `%s` |\n", input.SessionID))
	b.WriteString(fmt.Sprintf("| Worker | `%s` |\n", input.WorkerID))
	b.WriteString(fmt.Sprintf("| Retry | %d/%d |\n", input.RetryCount, input.MaxRetries))

	// PR link if available.
	prURL := extractPRURL(input)
	if prURL != "" {
		b.WriteString(fmt.Sprintf("\n**Pull Request**: %s\n", prURL))
	}

	// Retry limit warning.
	if input.RetryCount >= input.MaxRetries && input.MaxRetries > 0 {
		b.WriteString("\n> **Retry limit reached, needs human intervention**\n")
	}

	// Stdout/stderr output.
	if input.Result != nil {
		if input.Result.Stdout != "" {
			b.WriteString("\n### Stdout\n\n")
			b.WriteString(foldOutput(input.Result.Stdout, 200))
		}
		if input.Result.Stderr != "" {
			b.WriteString("\n### Stderr\n\n")
			b.WriteString(foldOutput(input.Result.Stderr, 200))
		}
	}

	// Footer.
	b.WriteString(fmt.Sprintf("\n---\n*workbuddy coordinator | %s*\n",
		time.Now().UTC().Format("2006-01-02 15:04:05 UTC")))

	return b.String()
}

// statusToIcon returns a Markdown-friendly status indicator.
func statusToIcon(status string) string {
	switch status {
	case "success":
		return "[SUCCESS]"
	case "failure":
		return "[FAILURE]"
	case "timeout":
		return "[TIMEOUT]"
	default:
		return "[UNKNOWN]"
	}
}

// formatDuration formats a duration for human display.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

// foldOutput wraps long output in <details> tags.
func foldOutput(output string, maxLines int) string {
	lines := strings.Split(output, "\n")
	if len(lines) <= maxLines {
		return "```\n" + output + "\n```\n"
	}
	return fmt.Sprintf("<details><summary>Output (%d lines, showing first %d)</summary>\n\n```\n%s\n```\n\n</details>\n",
		len(lines), maxLines, strings.Join(lines[:maxLines], "\n"))
}

// prURLRegex matches GitHub PR URLs in stdout.
var prURLRegex = regexp.MustCompile(`https://github\.com/[^\s]+/pull/\d+`)

// extractPRURL tries to find a PR URL from Result.Meta["pr_url"] first,
// then falls back to regex matching stdout.
func extractPRURL(input ReportInput) string {
	if input.Result == nil {
		return ""
	}
	if url, ok := input.Result.Meta["pr_url"]; ok && url != "" {
		return url
	}
	// Fallback: regex match in stdout.
	if match := prURLRegex.FindString(input.Result.Stdout); match != "" {
		return match
	}
	return ""
}
