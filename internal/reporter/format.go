// Package reporter formats and posts agent execution reports to GitHub issues.
package reporter

import (
	"fmt"
	"strings"
	"time"
)

// maxOutputLines is the threshold above which output is collapsed with <details>.
const maxOutputLines = 200

// ReportData holds the inputs for formatting an execution report.
type ReportData struct {
	AgentName    string
	Status       string // "success", "partial", "failure", "timeout", "retry-limit"
	Duration     time.Duration
	SessionID    string
	WorkerID     string
	RetryCount   int
	MaxRetries   int
	Output       string // combined stdout/stderr
	PRLink       string // optional PR URL
	ErrorDetail  string // optional error message
	SessionURL   string // optional URL to session detail page
	LabelLine    string // optional label validation summary
	Verification *VerificationResult
}

// statusBadge returns a Markdown status badge string.
func statusBadge(status string) string {
	switch status {
	case "success":
		return "**Status**: :white_check_mark: Success"
	case "partial":
		return "**Status**: :warning: Partial (claimed side-effects not verified)"
	case "failure":
		return "**Status**: :x: Failure"
	case "timeout":
		return "**Status**: :hourglass: Timeout"
	case "retry-limit":
		return "**Status**: :rotating_light: Retry Limit Reached"
	default:
		return fmt.Sprintf("**Status**: %s", status)
	}
}

// FormatReport generates a rich Markdown report from the given data.
func FormatReport(d ReportData) string {
	return FormatReportAt(d, time.Now())
}

// FormatReportAt is like FormatReport but accepts an explicit timestamp (for testing).
func FormatReportAt(d ReportData, ts time.Time) string {
	var b strings.Builder

	// Header
	fmt.Fprintf(&b, "## Agent Report: %s\n\n", d.AgentName)

	// Status badge
	b.WriteString(statusBadge(d.Status))
	b.WriteString("\n\n")

	// Metadata table
	b.WriteString("| Field | Value |\n")
	b.WriteString("|-------|-------|\n")
	fmt.Fprintf(&b, "| Agent | `%s` |\n", d.AgentName)
	fmt.Fprintf(&b, "| Duration | %s |\n", formatDuration(d.Duration))
	fmt.Fprintf(&b, "| Session ID | `%s` |\n", d.SessionID)
	fmt.Fprintf(&b, "| Worker | `%s` |\n", d.WorkerID)
	fmt.Fprintf(&b, "| Retry | %d / %d |\n", d.RetryCount, d.MaxRetries)
	b.WriteString("\n")

	// PR link if present
	if d.PRLink != "" {
		fmt.Fprintf(&b, ":link: **Pull Request**: %s\n\n", d.PRLink)
	}

	// Session detail link if present
	if d.SessionURL != "" {
		fmt.Fprintf(&b, ":mag: **[View Session Details](%s)**\n\n", d.SessionURL)
	}

	if d.LabelLine != "" {
		b.WriteString(d.LabelLine)
		b.WriteString("\n\n")
	}

	// Verification claim vs. reality table
	if d.Verification != nil && len(d.Verification.Checks) > 0 {
		b.WriteString("### Claim Verification\n\n")
		b.WriteString("| Claim | Actual | Status |\n")
		b.WriteString("|-------|--------|--------|\n")
		for _, c := range d.Verification.Checks {
			status := ":white_check_mark:"
			if !c.OK {
				status = ":x:"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", c.Claim, c.Actual, status)
		}
		b.WriteString("\n")
	}

	// Error detail for failure/timeout/retry-limit/partial
	if d.ErrorDetail != "" {
		b.WriteString("### Error\n\n")
		b.WriteString("```\n")
		b.WriteString(d.ErrorDetail)
		b.WriteString("\n```\n\n")
	}

	// Retry-limit specific message
	if d.Status == "retry-limit" {
		b.WriteString("> :warning: **Retry limit reached, needs human intervention**\n\n")
	}

	// Output section
	if d.Output != "" {
		lines := strings.Split(d.Output, "\n")
		if len(lines) > maxOutputLines {
			b.WriteString("<details>\n")
			fmt.Fprintf(&b, "<summary>Agent output (%d lines, click to expand)</summary>\n\n", len(lines))
			b.WriteString("```\n")
			b.WriteString(d.Output)
			b.WriteString("\n```\n\n")
			b.WriteString("</details>\n\n")
		} else {
			b.WriteString("### Output\n\n")
			b.WriteString("```\n")
			b.WriteString(d.Output)
			b.WriteString("\n```\n\n")
		}
	}

	// Footer with signature and timestamp
	b.WriteString("---\n")
	fmt.Fprintf(&b, "*workbuddy coordinator | %s*\n", ts.UTC().Format(time.RFC3339))

	return b.String()
}

func FormatNeedsHumanReport(labelLine string, ts time.Time) string {
	var b strings.Builder

	b.WriteString("## Managed Follow-up\n\n")
	b.WriteString("The agent exited successfully but did not change the workflow label.\n\n")
	if labelLine != "" {
		b.WriteString(labelLine)
		b.WriteString("\n\n")
	}
	b.WriteString("Recommended next step: add `needs-human` and review the issue manually.\n\n")

	b.WriteString("---\n")
	fmt.Fprintf(&b, "*workbuddy coordinator | %s*\n", ts.UTC().Format(time.RFC3339))

	return b.String()
}

// FormatStartedReport generates a Markdown "Agent Started" notification comment.
func FormatStartedReport(agentName, sessionID, workerID, sessionURL string, ts time.Time) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## :robot: Agent Started: %s\n\n", agentName)

	b.WriteString("| Field | Value |\n")
	b.WriteString("|-------|-------|\n")
	fmt.Fprintf(&b, "| Agent | `%s` |\n", agentName)
	fmt.Fprintf(&b, "| Session ID | `%s` |\n", sessionID)
	fmt.Fprintf(&b, "| Worker | `%s` |\n", workerID)
	fmt.Fprintf(&b, "| Started | %s |\n", ts.UTC().Format(time.RFC3339))
	b.WriteString("\n")

	if sessionURL != "" {
		fmt.Fprintf(&b, ":mag: **[View Live Session](%s)**\n\n", sessionURL)
	}

	b.WriteString("---\n")
	fmt.Fprintf(&b, "*workbuddy coordinator | %s*\n", ts.UTC().Format(time.RFC3339))

	return b.String()
}

// formatDuration returns a human-readable duration string.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.String()
	}
	// Round to seconds for readability
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	if seconds == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dm%ds", minutes, seconds)
}
