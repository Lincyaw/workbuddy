package reporter

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/Lincyaw/workbuddy/internal/launcher"
)

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

// Reporter writes execution reports as GitHub Issue comments.
type Reporter struct {
	gh      GHCommentWriter
	baseURL string // e.g. "http://localhost:8080", empty to omit session links
}

// NewReporter creates a Reporter with the given GH comment writer.
func NewReporter(gh GHCommentWriter) *Reporter {
	return &Reporter{gh: gh}
}

// SetBaseURL sets the base URL for session detail links in reports.
func (r *Reporter) SetBaseURL(baseURL string) {
	r.baseURL = baseURL
}

// Report formats and posts an agent execution report to the issue.
func (r *Reporter) Report(repo string, issueNum int, agentName string, result *launcher.Result,
	sessionID, workerID string, retryCount, maxRetries int) error {

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

	output := result.Stdout
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
	}

	body := FormatReportAt(data, time.Now())
	return r.gh.WriteComment(repo, issueNum, body)
}
