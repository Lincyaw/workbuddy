package reporter

import (
	"strings"
	"testing"
	"time"
)

var fixedTime = time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)

func TestFormatReport_Success(t *testing.T) {
	d := ReportData{
		AgentName:  "code-dev",
		Status:     "success",
		Duration:   2*time.Minute + 15*time.Second,
		SessionID:  "sess-abc-123",
		WorkerID:   "embedded-host1",
		RetryCount: 0,
		MaxRetries: 3,
		Output:     "All tests passed.\nBuild successful.",
	}

	md := FormatReportAt(d, fixedTime)

	// Check header
	if !strings.Contains(md, "## Agent Report: code-dev") {
		t.Error("missing header with agent name")
	}
	// Check status badge
	if !strings.Contains(md, ":white_check_mark: Success") {
		t.Error("missing success badge")
	}
	// Check metadata table fields
	for _, want := range []string{
		"| Agent | `code-dev` |",
		"| Duration | 2m15s |",
		"| Session ID | `sess-abc-123` |",
		"| Worker | `embedded-host1` |",
		"| Retry | 0 / 3 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing metadata: %s", want)
		}
	}
	// Check output is NOT folded (short)
	if strings.Contains(md, "<details>") {
		t.Error("short output should not be folded")
	}
	if !strings.Contains(md, "All tests passed.") {
		t.Error("missing output content")
	}
	// Check footer
	if !strings.Contains(md, "workbuddy coordinator") {
		t.Error("missing coordinator signature")
	}
	if !strings.Contains(md, "2026-01-15T10:30:00Z") {
		t.Error("missing timestamp in footer")
	}
	// Should NOT have error section
	if strings.Contains(md, "### Error") {
		t.Error("success report should not have error section")
	}
}

func TestFormatReport_Failure(t *testing.T) {
	d := ReportData{
		AgentName:   "code-test",
		Status:      "failure",
		Duration:    45 * time.Second,
		SessionID:   "sess-def-456",
		WorkerID:    "embedded-host2",
		RetryCount:  1,
		MaxRetries:  3,
		Output:      "test output here",
		ErrorDetail: "exit code 1: test failed",
	}

	md := FormatReportAt(d, fixedTime)

	if !strings.Contains(md, ":x: Failure") {
		t.Error("missing failure badge")
	}
	if !strings.Contains(md, "### Error") {
		t.Error("failure report should have error section")
	}
	if !strings.Contains(md, "exit code 1: test failed") {
		t.Error("missing error detail")
	}
	if !strings.Contains(md, "| Retry | 1 / 3 |") {
		t.Error("missing retry count")
	}
	// Should NOT have retry-limit warning
	if strings.Contains(md, "Retry limit reached") {
		t.Error("failure report should not have retry-limit warning")
	}
}

func TestFormatReport_Timeout(t *testing.T) {
	d := ReportData{
		AgentName:   "code-review",
		Status:      "timeout",
		Duration:    5 * time.Minute,
		SessionID:   "sess-ghi-789",
		WorkerID:    "embedded-host1",
		RetryCount:  2,
		MaxRetries:  3,
		Output:      "partial output before timeout...",
		ErrorDetail: "agent execution exceeded 5m timeout",
	}

	md := FormatReportAt(d, fixedTime)

	if !strings.Contains(md, ":hourglass: Timeout") {
		t.Error("missing timeout badge")
	}
	if !strings.Contains(md, "| Duration | 5m |") {
		t.Error("missing duration")
	}
	if !strings.Contains(md, "agent execution exceeded 5m timeout") {
		t.Error("missing timeout error detail")
	}
	if !strings.Contains(md, "workbuddy coordinator") {
		t.Error("missing footer signature")
	}
}

func TestFormatReport_RetryLimit(t *testing.T) {
	d := ReportData{
		AgentName:   "code-dev",
		Status:      "retry-limit",
		Duration:    1*time.Minute + 30*time.Second,
		SessionID:   "sess-jkl-012",
		WorkerID:    "embedded-host3",
		RetryCount:  3,
		MaxRetries:  3,
		Output:      "last attempt output",
		ErrorDetail: "max retries exceeded",
	}

	md := FormatReportAt(d, fixedTime)

	if !strings.Contains(md, ":rotating_light: Retry Limit Reached") {
		t.Error("missing retry-limit badge")
	}
	if !strings.Contains(md, "| Retry | 3 / 3 |") {
		t.Error("missing retry count at max")
	}
	if !strings.Contains(md, "Retry limit reached, needs human intervention") {
		t.Error("missing retry-limit human intervention warning")
	}
	if !strings.Contains(md, "workbuddy coordinator") {
		t.Error("missing footer signature")
	}
}

func TestFormatReport_LongOutputFolded(t *testing.T) {
	// Generate output with >200 lines
	var lines []string
	for i := 0; i < 250; i++ {
		lines = append(lines, "line content here")
	}
	longOutput := strings.Join(lines, "\n")

	d := ReportData{
		AgentName:  "code-dev",
		Status:     "success",
		Duration:   30 * time.Second,
		SessionID:  "sess-long-001",
		WorkerID:   "embedded-host1",
		RetryCount: 0,
		MaxRetries: 3,
		Output:     longOutput,
	}

	md := FormatReportAt(d, fixedTime)

	if !strings.Contains(md, "<details>") {
		t.Error("long output should be wrapped in <details>")
	}
	if !strings.Contains(md, "<summary>Agent output (250 lines, click to expand)</summary>") {
		t.Error("missing details summary with line count")
	}
	if !strings.Contains(md, "</details>") {
		t.Error("missing closing </details>")
	}
}

func TestFormatReport_PRLink(t *testing.T) {
	d := ReportData{
		AgentName:  "code-dev",
		Status:     "success",
		Duration:   1 * time.Minute,
		SessionID:  "sess-pr-001",
		WorkerID:   "embedded-host1",
		RetryCount: 0,
		MaxRetries: 3,
		PRLink:     "https://github.com/owner/repo/pull/42",
	}

	md := FormatReportAt(d, fixedTime)

	if !strings.Contains(md, "https://github.com/owner/repo/pull/42") {
		t.Error("missing PR link")
	}
	if !strings.Contains(md, "Pull Request") {
		t.Error("missing PR link label")
	}
}

func TestFormatReport_LabelLine(t *testing.T) {
	d := ReportData{
		AgentName:  "code-dev",
		Status:     "success",
		Duration:   1 * time.Minute,
		SessionID:  "sess-label-001",
		WorkerID:   "embedded-host1",
		RetryCount: 0,
		MaxRetries: 3,
		LabelLine:  "Label transition: developing -> reviewing (OK)",
	}

	md := FormatReportAt(d, fixedTime)

	if !strings.Contains(md, d.LabelLine) {
		t.Fatal("missing label transition line")
	}
}

func TestFormatNeedsHumanReport(t *testing.T) {
	labelLine := "Label transition: none - needs human review"
	md := FormatNeedsHumanReport(labelLine, fixedTime)

	if !strings.Contains(md, "Managed Follow-up") {
		t.Fatal("missing header")
	}
	if !strings.Contains(md, labelLine) {
		t.Fatal("missing label line")
	}
	if !strings.Contains(md, "needs-human") {
		t.Fatal("missing needs-human recommendation")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "500ms"},
		{5 * time.Second, "5s"},
		{2*time.Minute + 30*time.Second, "2m30s"},
		{3 * time.Minute, "3m"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
