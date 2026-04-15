package reporter

import (
	"strings"
	"testing"
	"time"
)

var fixedTime = time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)

// TestFormatReport_StatusBranching exercises the real branches in FormatReportAt:
// status-driven badge selection, error-section presence, and retry-limit warning.
// Literal table cells and footer strings are intentionally NOT asserted — those
// are template duplication, not behavior.
func TestFormatReport_StatusBranching(t *testing.T) {
	base := ReportData{
		AgentName:  "code-dev",
		SessionID:  "sess",
		WorkerID:   "worker",
		Duration:   time.Second,
		MaxRetries: 3,
		Output:     "short output",
	}
	tests := []struct {
		status          string
		errorDetail     string
		retryCount      int
		wantErrSection  bool
		wantRetryLimit  bool
	}{
		{"success", "", 0, false, false},
		{"failure", "exit code 1", 1, true, false},
		{"timeout", "deadline exceeded", 2, true, false},
		{"retry-limit", "max retries exceeded", 3, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			d := base
			d.Status = tt.status
			d.ErrorDetail = tt.errorDetail
			d.RetryCount = tt.retryCount
			md := FormatReportAt(d, fixedTime)

			hasErr := strings.Contains(md, "### Error")
			if hasErr != tt.wantErrSection {
				t.Errorf("error section = %v, want %v", hasErr, tt.wantErrSection)
			}
			hasWarn := strings.Contains(md, "Retry limit reached")
			if hasWarn != tt.wantRetryLimit {
				t.Errorf("retry-limit warning = %v, want %v", hasWarn, tt.wantRetryLimit)
			}
			if tt.errorDetail != "" && !strings.Contains(md, tt.errorDetail) {
				t.Errorf("error detail %q missing from output", tt.errorDetail)
			}
		})
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
