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
	Status       string // "success", "partial", "failure", "timeout", "retry-limit", "infra-error"
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
	// InfraReason is a short operator-facing string describing why this run
	// was classified as an infra failure (exec error, scanner overflow,
	// runtime panic, etc.). Only set when Status == "infra-error".
	InfraReason string
	// SyncFailure describes a coordinator-sync failure that happened after the
	// agent finished locally. This keeps submit/release transport problems out
	// of ad-hoc string payloads in worker code.
	SyncFailure *SyncFailure
}

// SyncFailure captures a typed transport-layer sync failure for report
// formatting.
type SyncFailure struct {
	Operation string
	Detail    string
}

// StartedData holds the canonical facts for an "Agent Started" comment.
type StartedData struct {
	AgentName  string
	SessionID  string
	WorkerID   string
	SessionURL string
	StartedAt  time.Time
}

// NeedsHumanData holds the canonical facts for a needs-human recommendation.
type NeedsHumanData struct {
	LabelLine string
	Timestamp time.Time
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
	case "infra-error":
		return "**Status**: :construction: Infra Error (launcher-layer, not an agent verdict)"
	default:
		return fmt.Sprintf("**Status**: %s", status)
	}
}

// reportHeader returns the H2 header line for a report. Infra failures get a
// distinct "Infra Error" header so operators do not read them as agent
// FAIL verdicts.
func reportHeader(agentName, status string) string {
	if status == "infra-error" {
		return fmt.Sprintf("## Infra Error: %s\n\n", agentName)
	}
	return fmt.Sprintf("## Agent Report: %s\n\n", agentName)
}

// FormatReport generates a rich Markdown report from the given data.
func FormatReport(d ReportData) string {
	return FormatReportAt(d, time.Now())
}

// FormatReportAt is like FormatReport but accepts an explicit timestamp (for testing).
func FormatReportAt(d ReportData, ts time.Time) string {
	var b strings.Builder

	// Header — "Infra Error" for infra-layer failures, "Agent Report" otherwise.
	b.WriteString(reportHeader(d.AgentName, d.Status))

	// Status badge
	b.WriteString(statusBadge(d.Status))
	b.WriteString("\n\n")

	// Infra-error explanation block: surface the launcher reason and make it
	// explicit that this was NOT an agent verdict, so operators triaging the
	// issue do not think "the agent disagreed with itself".
	if d.Status == "infra-error" {
		b.WriteString("> :construction: **This was not an agent verdict.** ")
		b.WriteString("The launcher failed to run the agent (e.g. exec error, scanner buffer overflow, runtime panic before any agent output). ")
		b.WriteString("The state machine is NOT being told the agent FAILED; retries are bounded by the dispatch failure cap only.\n\n")
		if d.InfraReason != "" {
			fmt.Fprintf(&b, "**Launcher reason**: `%s`\n\n", d.InfraReason)
		}
	}

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

	if d.SyncFailure != nil {
		b.WriteString("### Coordinator Sync\n\n")
		fmt.Fprintf(&b, "- Operation: `%s`\n", d.SyncFailure.Operation)
		if d.SyncFailure.Detail != "" {
			fmt.Fprintf(&b, "- Result: %s\n", d.SyncFailure.Detail)
		}
		b.WriteString("\n")
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

func FormatNeedsHumanReport(d NeedsHumanData) string {
	var b strings.Builder

	b.WriteString("## Managed Follow-up\n\n")
	b.WriteString("The agent exited successfully but did not change the workflow label.\n\n")
	if d.LabelLine != "" {
		b.WriteString(d.LabelLine)
		b.WriteString("\n\n")
	}
	b.WriteString("Recommended next step: add `needs-human` and review the issue manually.\n\n")

	b.WriteString("---\n")
	fmt.Fprintf(&b, "*workbuddy coordinator | %s*\n", d.Timestamp.UTC().Format(time.RFC3339))

	return b.String()
}

// FormatStartedReport generates a Markdown "Agent Started" notification comment.
func FormatStartedReport(d StartedData) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## :robot: Agent Started: %s\n\n", d.AgentName)

	b.WriteString("| Field | Value |\n")
	b.WriteString("|-------|-------|\n")
	fmt.Fprintf(&b, "| Agent | `%s` |\n", d.AgentName)
	fmt.Fprintf(&b, "| Session ID | `%s` |\n", d.SessionID)
	fmt.Fprintf(&b, "| Worker | `%s` |\n", d.WorkerID)
	fmt.Fprintf(&b, "| Started | %s |\n", d.StartedAt.UTC().Format(time.RFC3339))
	b.WriteString("\n")

	if d.SessionURL != "" {
		fmt.Fprintf(&b, ":mag: **[View Live Session](%s)**\n\n", d.SessionURL)
	}

	b.WriteString("---\n")
	fmt.Fprintf(&b, "*workbuddy coordinator | %s*\n", d.StartedAt.UTC().Format(time.RFC3339))

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
