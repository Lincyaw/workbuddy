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
		SessionURL: "http://127.0.0.1:8090/sessions/session-123",
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
		SessionURL:  "http://127.0.0.1:8090/sessions/session-123",
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
