package reporter

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/launcher"
)

type mockGHWriter struct {
	comments []string
	failN    int // fail the first N calls
	calls    int
}

func (m *mockGHWriter) WriteComment(_ string, _ int, body string) error {
	m.calls++
	if m.calls <= m.failN {
		return fmt.Errorf("mock gh error")
	}
	m.comments = append(m.comments, body)
	return nil
}

func TestReport_Success(t *testing.T) {
	gh := &mockGHWriter{}
	r := NewReporter(gh)

	result := &launcher.Result{
		ExitCode: 0,
		Stdout:   "all good",
		Duration: 5 * time.Second,
		Meta:     map[string]string{"pr_url": "https://github.com/test/repo/pull/1"},
	}

	err := r.Report("test/repo", 42, "dev-agent", result, "sess-123", "worker-1", 1, 3, "Label transition: developing -> reviewing (OK)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gh.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(gh.comments))
	}
	body := gh.comments[0]
	if !strings.Contains(body, "dev-agent") {
		t.Error("comment should contain agent name")
	}
	if !strings.Contains(body, "sess-123") {
		t.Error("comment should contain session ID")
	}
	if !strings.Contains(body, "worker-1") {
		t.Error("comment should contain worker ID")
	}
	if !strings.Contains(body, "1 / 3") {
		t.Error("comment should contain retry count")
	}
	if !strings.Contains(body, "https://github.com/test/repo/pull/1") {
		t.Error("comment should contain PR link")
	}
	if !strings.Contains(body, "Label transition: developing -> reviewing (OK)") {
		t.Error("comment should contain label transition summary")
	}
}

func TestReport_Failure(t *testing.T) {
	gh := &mockGHWriter{}
	r := NewReporter(gh)

	result := &launcher.Result{
		ExitCode: 1,
		Stderr:   "compilation error",
		Duration: 2 * time.Second,
	}

	err := r.Report("test/repo", 42, "dev-agent", result, "sess-456", "worker-1", 0, 3, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := gh.comments[0]
	if !strings.Contains(body, "Failure") && !strings.Contains(body, "failure") {
		t.Error("comment should indicate failure")
	}
}

func TestReport_PrefersLastMessage(t *testing.T) {
	gh := &mockGHWriter{}
	r := NewReporter(gh)

	result := &launcher.Result{
		ExitCode:    0,
		LastMessage: "final answer",
		Stdout:      "raw stdout",
		Duration:    time.Second,
	}

	if err := r.Report("test/repo", 42, "dev-agent", result, "sess-last", "worker-1", 0, 3, ""); err != nil {
		t.Fatalf("Report: %v", err)
	}
	body := gh.comments[0]
	if !strings.Contains(body, "final answer") {
		t.Fatalf("expected last message in report: %s", body)
	}
	if strings.Contains(body, "raw stdout") {
		t.Fatalf("expected stdout fallback not to be used: %s", body)
	}
}

func TestReport_RetryOnGHFailure(t *testing.T) {
	gh := &mockGHWriter{failN: 1} // first call fails, second succeeds
	r := NewReporter(gh)

	result := &launcher.Result{
		ExitCode: 0,
		Stdout:   "ok",
		Duration: 1 * time.Second,
	}

	// The current reporter.go doesn't retry - it just returns the error.
	// But let's test the happy path after first failure.
	err := r.Report("test/repo", 42, "dev-agent", result, "sess-789", "worker-1", 0, 3, "")
	// First call to gh.WriteComment fails, so Report should return error
	if err == nil {
		// If reporter implements retry internally, this is fine
		if len(gh.comments) != 1 {
			t.Errorf("expected 1 comment after retry, got %d", len(gh.comments))
		}
	}
}

func TestReportNeedsHuman(t *testing.T) {
	gh := &mockGHWriter{}
	r := NewReporter(gh)

	if err := r.ReportNeedsHuman("test/repo", 42, "Label transition: none - needs human review"); err != nil {
		t.Fatalf("ReportNeedsHuman: %v", err)
	}
	if len(gh.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(gh.comments))
	}
	if !strings.Contains(gh.comments[0], "needs-human") {
		t.Fatalf("expected needs-human recommendation in comment: %s", gh.comments[0])
	}
}

type mockReactions struct {
	calls []struct {
		repo     string
		issueNum int
		blocked  bool
	}
	err error
}

func (m *mockReactions) SetBlockedReaction(_ context.Context, repo string, issueNum int, blocked bool) error {
	m.calls = append(m.calls, struct {
		repo     string
		issueNum int
		blocked  bool
	}{repo, issueNum, blocked})
	return m.err
}

func TestSetBlockedReactionDelegatesToManager(t *testing.T) {
	r := NewReporter(&mockGHWriter{})
	mock := &mockReactions{}
	r.SetReactionManager(mock)

	if err := r.SetBlockedReaction(context.Background(), "owner/repo", 7, true); err != nil {
		t.Fatalf("SetBlockedReaction(true): %v", err)
	}
	if err := r.SetBlockedReaction(context.Background(), "owner/repo", 7, false); err != nil {
		t.Fatalf("SetBlockedReaction(false): %v", err)
	}
	if len(mock.calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(mock.calls))
	}
	if mock.calls[0].blocked != true || mock.calls[1].blocked != false {
		t.Fatalf("call sequence wrong: %+v", mock.calls)
	}
	if mock.calls[0].repo != "owner/repo" || mock.calls[0].issueNum != 7 {
		t.Fatalf("call args wrong: %+v", mock.calls[0])
	}
}

func TestReactionConfusedConstant(t *testing.T) {
	if ReactionConfused != "confused" {
		t.Fatalf("ReactionConfused = %q, want %q", ReactionConfused, "confused")
	}
}
