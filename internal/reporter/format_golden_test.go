package reporter

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFormatStartedReport_Golden(t *testing.T) {
	got := FormatStartedReport(StartedData{
		AgentName:  "dev-agent",
		SessionID:  "session-123",
		WorkerID:   "worker-a",
		SessionURL: "http://127.0.0.1:8090/workers/worker-a/sessions/session-123",
		StartedAt:  time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC),
	})
	assertGolden(t, "started.md", got)
}

func TestFormatNeedsHumanReport_Golden(t *testing.T) {
	got := FormatNeedsHumanReport(NeedsHumanData{
		LabelLine: "Label validation: no workflow label transition detected.",
		Timestamp: time.Date(2026, 4, 29, 12, 1, 0, 0, time.UTC),
	})
	assertGolden(t, "needs-human.md", got)
}

func TestFormatSynthesisNeedsHumanReport_Golden(t *testing.T) {
	got := FormatSynthesisNeedsHumanReport(SynthesisNeedsHumanData{
		Reason:    "malformed_or_missing_synthesis_output",
		Timestamp: time.Date(2026, 4, 29, 12, 1, 30, 0, time.UTC),
	})
	assertGolden(t, "synthesis-needs-human.md", got)
}

func TestFormatReportAt_Golden(t *testing.T) {
	got := FormatReportAt(ReportData{
		AgentName:   "dev-agent",
		Status:      "partial",
		Duration:    93 * time.Second,
		SessionID:   "session-123",
		WorkerID:    "worker-a",
		RetryCount:  1,
		MaxRetries:  3,
		Output:      "line 1\nline 2",
		PRLink:      "https://github.com/Lincyaw/workbuddy/pull/210",
		ErrorDetail: "Claim verification failed:\n- pr_created: claimed \"210\", actual \"none\"",
		SessionURL:  "http://127.0.0.1:8090/workers/worker-a/sessions/session-123",
		LabelLine:   "Label validation: no workflow label transition detected.",
		Verification: &VerificationResult{Checks: []ClaimCheck{{
			Type:   ClaimPRCreated,
			Claim:  "created PR #210",
			Actual: "no matching PR",
			OK:     false,
		}}},
		SyncFailure: &SyncFailure{Operation: "submit_result", Detail: "coordinator returned 502"},
	}, time.Date(2026, 4, 29, 12, 2, 0, 0, time.UTC))
	assertGolden(t, "report-partial-sync.md", got)
}

func TestFormatReportAtSuccess_Golden(t *testing.T) {
	got := FormatReportAt(ReportData{
		AgentName:  "dev-agent",
		Status:     "success",
		Duration:   47 * time.Second,
		SessionID:  "session-123",
		WorkerID:   "worker-a",
		RetryCount: 0,
		MaxRetries: 3,
		Output:     "implemented feature\nadded tests",
		PRLink:     "https://github.com/Lincyaw/workbuddy/pull/210",
		SessionURL: "http://127.0.0.1:8090/workers/worker-a/sessions/session-123",
		LabelLine:  "Label validation: transitioned from `status:developing` to `status:reviewing`.",
		Verification: &VerificationResult{Checks: []ClaimCheck{{
			Type:   ClaimPRCreated,
			Claim:  "created PR #210",
			Actual: "PR #210 found",
			OK:     true,
		}}},
	}, time.Date(2026, 4, 29, 12, 3, 0, 0, time.UTC))
	assertGolden(t, "report-success.md", got)
}

func TestFormatReportAtFailure_Golden(t *testing.T) {
	got := FormatReportAt(ReportData{
		AgentName:   "dev-agent",
		Status:      "failure",
		Duration:    2*time.Minute + 5*time.Second,
		SessionID:   "session-123",
		WorkerID:    "worker-a",
		RetryCount:  2,
		MaxRetries:  3,
		Output:      "running tests\nfailure: panic in worker",
		ErrorDetail: "exit status 1",
		SessionURL:  "http://127.0.0.1:8090/workers/worker-a/sessions/session-123",
		LabelLine:   "Label validation: workflow label stayed at `status:developing`.",
	}, time.Date(2026, 4, 29, 12, 4, 0, 0, time.UTC))
	assertGolden(t, "report-failure.md", got)
}

func TestFormatReportAtTimeout_Golden(t *testing.T) {
	got := FormatReportAt(ReportData{
		AgentName:   "dev-agent",
		Status:      "timeout",
		Duration:    30 * time.Minute,
		SessionID:   "session-123",
		WorkerID:    "worker-a",
		RetryCount:  1,
		MaxRetries:  3,
		ErrorDetail: "agent exceeded 30m timeout",
		SessionURL:  "http://127.0.0.1:8090/workers/worker-a/sessions/session-123",
	}, time.Date(2026, 4, 29, 12, 5, 0, 0, time.UTC))
	assertGolden(t, "report-timeout.md", got)
}

func TestFormatReportAtRetryLimit_Golden(t *testing.T) {
	got := FormatReportAt(ReportData{
		AgentName:   "dev-agent",
		Status:      "retry-limit",
		Duration:    3 * time.Second,
		SessionID:   "session-123",
		WorkerID:    "worker-a",
		RetryCount:  3,
		MaxRetries:  3,
		ErrorDetail: "three attempts exhausted",
		SessionURL:  "http://127.0.0.1:8090/workers/worker-a/sessions/session-123",
	}, time.Date(2026, 4, 29, 12, 6, 0, 0, time.UTC))
	assertGolden(t, "report-retry-limit.md", got)
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", name, got, string(want))
	}
}
