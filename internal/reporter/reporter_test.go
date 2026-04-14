package reporter

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/launcher"
)

// --- Fakes ---

type fakeGHWriter struct {
	mu       sync.Mutex
	comments []postedComment
	failN    int // fail the first N calls
	called   int
}

type postedComment struct {
	Repo     string
	IssueNum int
	Body     string
}

func (f *fakeGHWriter) CommentOnIssue(repo string, issueNum int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	if f.called <= f.failN {
		return fmt.Errorf("gh: command failed (attempt %d)", f.called)
	}
	f.comments = append(f.comments, postedComment{Repo: repo, IssueNum: issueNum, Body: body})
	return nil
}

type fakeEventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	Type     string
	Repo     string
	IssueNum int
	Payload  interface{}
}

func (f *fakeEventRecorder) Log(eventType, repo string, issueNum int, payload interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{
		Type: eventType, Repo: repo, IssueNum: issueNum, Payload: payload,
	})
}

func (f *fakeEventRecorder) findEvent(eventType string) *recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.events {
		if f.events[i].Type == eventType {
			return &f.events[i]
		}
	}
	return nil
}

// --- Tests ---

func TestFormatReport_Success(t *testing.T) {
	input := ReportInput{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
		WorkerID:  "worker-1",
		SessionID: "session-abc",
		Result: &launcher.Result{
			ExitCode: 0,
			Stdout:   "Build successful\nAll tests passed",
			Stderr:   "",
			Duration: 45 * time.Second,
			Meta:     map[string]string{"pr_url": "https://github.com/owner/repo/pull/99"},
		},
		RetryCount: 1,
		MaxRetries: 3,
		Status:     "success",
	}

	body := FormatReport(input)

	// Check required fields.
	checks := []string{
		"[SUCCESS]",
		"`dev-agent`",
		"`session-abc`",
		"`worker-1`",
		"1/3",
		"45.0s",
		"https://github.com/owner/repo/pull/99",
		"Build successful",
		"workbuddy coordinator",
	}
	for _, want := range checks {
		if !strings.Contains(body, want) {
			t.Errorf("report body missing %q\n\nFull body:\n%s", want, body)
		}
	}

	// Should NOT contain retry limit warning.
	if strings.Contains(body, "Retry limit reached") {
		t.Error("success report should not contain retry limit warning")
	}
}

func TestFormatReport_Failure(t *testing.T) {
	input := ReportInput{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "test-agent",
		WorkerID:  "worker-2",
		SessionID: "session-def",
		Result: &launcher.Result{
			ExitCode: 1,
			Stdout:   "Running tests...",
			Stderr:   "FAIL: TestSomething",
			Duration: 12 * time.Second,
			Meta:     map[string]string{},
		},
		RetryCount: 1,
		MaxRetries: 3,
		Status:     "failure",
	}

	body := FormatReport(input)

	checks := []string{
		"[FAILURE]",
		"`test-agent`",
		"FAIL: TestSomething",
		"### Stderr",
	}
	for _, want := range checks {
		if !strings.Contains(body, want) {
			t.Errorf("report body missing %q", want)
		}
	}
}

func TestFormatReport_Timeout(t *testing.T) {
	input := ReportInput{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
		WorkerID:  "worker-1",
		SessionID: "session-ghi",
		Result: &launcher.Result{
			ExitCode: -1,
			Stdout:   "",
			Stderr:   "context deadline exceeded",
			Duration: 5 * time.Minute,
			Meta:     map[string]string{},
		},
		RetryCount: 2,
		MaxRetries: 3,
		Status:     "timeout",
	}

	body := FormatReport(input)

	checks := []string{
		"[TIMEOUT]",
		"5m0s",
		"context deadline exceeded",
	}
	for _, want := range checks {
		if !strings.Contains(body, want) {
			t.Errorf("report body missing %q", want)
		}
	}
}

func TestFormatReport_RetryLimitReached(t *testing.T) {
	input := ReportInput{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
		WorkerID:  "worker-1",
		SessionID: "session-jkl",
		Result: &launcher.Result{
			ExitCode: 1,
			Stdout:   "Failed again",
			Stderr:   "",
			Duration: 30 * time.Second,
			Meta:     map[string]string{},
		},
		RetryCount: 3,
		MaxRetries: 3,
		Status:     "failure",
	}

	body := FormatReport(input)

	if !strings.Contains(body, "Retry limit reached, needs human intervention") {
		t.Error("report should contain retry limit warning when retryCount >= maxRetries")
	}
	if !strings.Contains(body, "3/3") {
		t.Error("report should show retry count 3/3")
	}
}

func TestFormatReport_LongOutputFolded(t *testing.T) {
	// Generate >200 lines of output.
	var lines []string
	for i := 0; i < 250; i++ {
		lines = append(lines, fmt.Sprintf("line %d: output data here", i))
	}
	longOutput := strings.Join(lines, "\n")

	input := ReportInput{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
		WorkerID:  "worker-1",
		SessionID: "session-mno",
		Result: &launcher.Result{
			ExitCode: 0,
			Stdout:   longOutput,
			Stderr:   "",
			Duration: 10 * time.Second,
			Meta:     map[string]string{},
		},
		RetryCount: 0,
		MaxRetries: 3,
		Status:     "success",
	}

	body := FormatReport(input)

	if !strings.Contains(body, "<details>") {
		t.Error("long output should be wrapped in <details> tags")
	}
	if !strings.Contains(body, "<summary>") {
		t.Error("long output should have a <summary> tag")
	}
	if !strings.Contains(body, "250 lines") {
		t.Errorf("summary should mention line count, got body:\n%s", body[:500])
	}
}

func TestFormatReport_PRFromStdout(t *testing.T) {
	input := ReportInput{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
		WorkerID:  "worker-1",
		SessionID: "session-pqr",
		Result: &launcher.Result{
			ExitCode: 0,
			Stdout:   "Created PR: https://github.com/owner/repo/pull/123\nDone.",
			Stderr:   "",
			Duration: 20 * time.Second,
			Meta:     map[string]string{}, // no pr_url in meta
		},
		RetryCount: 0,
		MaxRetries: 3,
		Status:     "success",
	}

	body := FormatReport(input)

	if !strings.Contains(body, "https://github.com/owner/repo/pull/123") {
		t.Error("report should extract PR URL from stdout via regex")
	}
}

func TestReporter_GHRetryOnFailure(t *testing.T) {
	// GH writer fails on first call, succeeds on second.
	gh := &fakeGHWriter{failN: 1}
	el := &fakeEventRecorder{}
	rep := NewReporter(gh, el)

	input := ReportInput{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
		WorkerID:  "worker-1",
		SessionID: "session-stu",
		Result: &launcher.Result{
			ExitCode: 0,
			Stdout:   "ok",
			Duration: 5 * time.Second,
			Meta:     map[string]string{},
		},
		RetryCount: 0,
		MaxRetries: 3,
		Status:     "success",
	}

	err := rep.Report(input)
	if err != nil {
		t.Fatalf("expected report to succeed after retry, got: %v", err)
	}

	if len(gh.comments) != 1 {
		t.Errorf("expected 1 comment posted after retry, got %d", len(gh.comments))
	}
	if gh.called != 2 {
		t.Errorf("expected 2 GH calls (1 fail + 1 retry), got %d", gh.called)
	}

	// Should have a report event logged.
	ev := el.findEvent("report")
	if ev == nil {
		t.Error("expected 'report' event to be logged")
	}
}

func TestReporter_GHBothAttemptsFail(t *testing.T) {
	// GH writer fails on both calls.
	gh := &fakeGHWriter{failN: 2}
	el := &fakeEventRecorder{}
	rep := NewReporter(gh, el)

	input := ReportInput{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
		WorkerID:  "worker-1",
		SessionID: "session-vwx",
		Result: &launcher.Result{
			ExitCode: 0,
			Stdout:   "ok",
			Duration: 5 * time.Second,
			Meta:     map[string]string{},
		},
		RetryCount: 0,
		MaxRetries: 3,
		Status:     "success",
	}

	err := rep.Report(input)
	if err == nil {
		t.Fatal("expected error when both GH attempts fail")
	}

	// Should have an error event logged.
	ev := el.findEvent("error")
	if ev == nil {
		t.Error("expected 'error' event to be logged")
	}
}
