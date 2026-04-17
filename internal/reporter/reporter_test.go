package reporter

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/launcher"
)

type mockVerifier struct {
	result *VerificationResult
	err    error
	calls  int
	inputs []VerificationInput
}

func (m *mockVerifier) Verify(_ context.Context, _ string, _ int, input VerificationInput) (*VerificationResult, error) {
	m.calls++
	m.inputs = append(m.inputs, input)
	return m.result, m.err
}

type mockGHWriter struct {
	comments []string
	failN    int // fail the first N calls
	calls    int
	err      error
}

func (m *mockGHWriter) WriteComment(_ string, _ int, body string) error {
	m.calls++
	if m.calls <= m.failN {
		if m.err != nil {
			return m.err
		}
		return fmt.Errorf("mock gh error: rate limit exceeded")
	}
	m.comments = append(m.comments, body)
	return nil
}

type mockEventRecorder struct {
	events []event
}

type event struct {
	eventType string
	repo      string
	issueNum  int
	payload   interface{}
}

func (m *mockEventRecorder) Log(eventType, repo string, issueNum int, payload interface{}) {
	m.events = append(m.events, event{eventType: eventType, repo: repo, issueNum: issueNum, payload: payload})
}

func (m *mockEventRecorder) Has(eventType string) bool {
	for _, e := range m.events {
		if e.eventType == eventType {
			return true
		}
	}
	return false
}

func (m *mockEventRecorder) findPayload(eventType string) (interface{}, bool) {
	for _, e := range m.events {
		if e.eventType == eventType {
			return e.payload, true
		}
	}
	return nil, false
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

	err := r.Report(context.Background(), "test/repo", 42, "dev-agent", result, "sess-123", "worker-1", 1, 3, "Label transition: developing -> reviewing (OK)")
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

	err := r.Report(context.Background(), "test/repo", 42, "dev-agent", result, "sess-456", "worker-1", 0, 3, "")
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

	if err := r.Report(context.Background(), "test/repo", 42, "dev-agent", result, "sess-last", "worker-1", 0, 3, ""); err != nil {
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

func TestReportWithVerification_Partial(t *testing.T) {
	gh := &mockGHWriter{}
	r := NewReporter(gh)
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	v := &mockVerifier{
		result: &VerificationResult{
			Partial: true,
			Checks: []ClaimCheck{
				{Type: ClaimCommentPR, Claim: "posted comment on PR #4", Actual: "no comments on PR", OK: false},
			},
		},
	}
	r.SetVerifier(v)

	result := &launcher.Result{
		ExitCode:    0,
		LastMessage: "I posted a review comment on PR #4",
		Duration:    time.Second,
	}

	verifyRes, err := r.ReportWithVerification(context.Background(), "test/repo", 42, "review-agent", result, "sess-verify", "worker-1", 0, 3, "")
	if err != nil {
		t.Fatalf("ReportWithVerification: %v", err)
	}
	if verifyRes == nil || !verifyRes.Partial {
		t.Fatalf("expected partial verification result, got %+v", verifyRes)
	}
	if v.calls != 1 {
		t.Fatalf("expected verifier to be called once, got %d", v.calls)
	}
	if len(v.inputs) != 1 {
		t.Fatalf("expected verifier input, got %d", len(v.inputs))
	}
	if v.inputs[0].StartedAt != now.Add(-time.Second) || v.inputs[0].EndedAt != now {
		t.Fatalf("unexpected run window: %+v", v.inputs[0])
	}
	body := gh.comments[0]
	if !strings.Contains(body, "Partial") {
		t.Fatalf("expected partial status in report: %s", body)
	}
	if !strings.Contains(body, "Claim Verification") {
		t.Fatalf("expected verification table in report: %s", body)
	}
	if !strings.Contains(body, "no comments on PR") {
		t.Fatalf("expected claim-vs-reality detail in report: %s", body)
	}
}

func TestReportVerified_UsesProvidedVerification(t *testing.T) {
	gh := &mockGHWriter{}
	r := NewReporter(gh)
	v := &mockVerifier{
		result: &VerificationResult{
			Partial: true,
			Checks:  []ClaimCheck{{Type: ClaimLabels, Claim: "added status:done", Actual: "label not found", OK: false}},
		},
	}
	r.SetVerifier(v)

	result := &launcher.Result{
		ExitCode:    0,
		LastMessage: "I flipped labels to status:done",
		Duration:    time.Second,
	}

	err := r.ReportVerified(context.Background(), "test/repo", 42, "dev-agent", result, "sess-precomputed", "worker-1", 0, 3, "", v.result)
	if err != nil {
		t.Fatalf("ReportVerified: %v", err)
	}
	if v.calls != 0 {
		t.Fatalf("expected provided verification to skip verifier, got %d calls", v.calls)
	}
	body := gh.comments[0]
	if !strings.Contains(body, "label not found") {
		t.Fatalf("expected provided verification detail in report: %s", body)
	}
}

func TestReporterVerify_UsesRunWindow(t *testing.T) {
	r := NewReporter(&mockGHWriter{})
	now := time.Date(2026, 4, 17, 13, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	v := &mockVerifier{result: &VerificationResult{}}
	r.SetVerifier(v)

	result := &launcher.Result{
		ExitCode:    0,
		LastMessage: "I posted a review comment on PR #4",
		Duration:    90 * time.Second,
	}

	if _, err := r.Verify(context.Background(), "test/repo", 42, result); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if v.calls != 1 {
		t.Fatalf("expected verifier to be called once, got %d", v.calls)
	}
	if got := v.inputs[0].Output; got != result.LastMessage {
		t.Fatalf("output = %q, want %q", got, result.LastMessage)
	}
	if got := v.inputs[0].StartedAt; got != now.Add(-90*time.Second) {
		t.Fatalf("startedAt = %s, want %s", got, now.Add(-90*time.Second))
	}
	if got := v.inputs[0].EndedAt; got != now {
		t.Fatalf("endedAt = %s, want %s", got, now)
	}
}

func TestReport_RetryOnRateLimit(t *testing.T) {
	gh := &mockGHWriter{failN: 2}
	r := NewReporter(gh)
	rec := &mockEventRecorder{}
	r.SetEventRecorder(rec)

	saved := rateLimitRetryDelays
	rateLimitRetryDelays = []time.Duration{1 * time.Millisecond}
	defer func() { rateLimitRetryDelays = saved }()
	result := &launcher.Result{
		ExitCode: 0,
		Stdout:   "ok",
		Duration: 1 * time.Second,
	}
	err := r.Report(context.Background(), "test/repo", 42, "dev-agent", result, "sess-789", "worker-1", 0, 3, "")
	if err != nil {
		t.Fatalf("report should succeed after retries, got: %v", err)
	}
	if gh.calls != 3 {
		t.Fatalf("expected 3 calls with retries, got %d", gh.calls)
	}
	if !rec.Has(eventlog.TypeRateLimit) {
		t.Fatalf("expected rate limit event to be recorded")
	}
}

func TestReport_RateLimitPayloadRedactsToken(t *testing.T) {
	ghWriter := &mockGHWriter{
		failN: 1,
		err:   fmt.Errorf("mock gh error: ghp_12345678901234567890: rate limit exceeded"),
	}
	r := NewReporter(ghWriter)
	rec := &mockEventRecorder{}
	r.SetEventRecorder(rec)

	saved := rateLimitRetryDelays
	rateLimitRetryDelays = []time.Duration{1 * time.Millisecond}
	defer func() { rateLimitRetryDelays = saved }()

	result := &launcher.Result{
		ExitCode: 0,
		Stdout:   "ok",
		Duration: 1 * time.Second,
	}
	err := r.Report(context.Background(), "test/repo", 42, "dev-agent", result, "sess-redact", "worker-1", 0, 3, "")
	if err != nil {
		t.Fatalf("report should succeed after retries, got: %v", err)
	}
	payload, ok := rec.findPayload(eventlog.TypeRateLimit)
	if !ok {
		t.Fatalf("expected rate limit event to be recorded")
	}
	p, ok := payload.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected rate limit payload type: %T", payload)
	}
	if p == nil {
		t.Fatalf("rate limit payload is empty")
	}
	if v, ok := p["error"].(string); ok {
		if strings.Contains(v, "ghp_12345678901234567890") {
			t.Fatalf("expected token redaction, got %q", v)
		}
	}
}

func TestReport_RetryCancelsWithContext(t *testing.T) {
	gh := &mockGHWriter{failN: 10}
	r := NewReporter(gh)
	saved := rateLimitRetryDelays
	rateLimitRetryDelays = []time.Duration{10 * time.Millisecond}
	defer func() { rateLimitRetryDelays = saved }()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1 * time.Millisecond)
		cancel()
	}()

	result := &launcher.Result{
		ExitCode: 0,
		Stdout:   "ok",
		Duration: 1 * time.Second,
	}
	err := r.Report(ctx, "test/repo", 42, "dev-agent", result, "sess-cancel", "worker-1", 0, 3, "")
	if err == nil {
		t.Fatal("expected report to fail when context canceled")
	}
	if err != context.Canceled {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if gh.calls == 0 {
		t.Fatalf("expected at least one write attempt before cancellation")
	}
	if gh.calls > 2 {
		t.Fatalf("expected at most two attempts with prompt cancellation, got %d", gh.calls)
	}
}

func TestReportNeedsHuman(t *testing.T) {
	gh := &mockGHWriter{}
	r := NewReporter(gh)

	if err := r.ReportNeedsHuman(context.Background(), "test/repo", 42, "Label transition: none - needs human review"); err != nil {
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

func TestReportStartedUsesWriter(t *testing.T) {
	gh := &mockGHWriter{}
	r := NewReporter(gh)

	err := r.ReportStarted(context.Background(), "test/repo", 42, "dev-agent", "sess-123", "worker-1")
	if err != nil {
		t.Fatalf("ReportStarted: %v", err)
	}
	if len(gh.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(gh.comments))
	}
	if !strings.Contains(gh.comments[0], "Session ID") {
		t.Fatalf("expected started report to include session id: %s", gh.comments[0])
	}
}

func TestReactionConfusedConstant(t *testing.T) {
	if ReactionConfused != "confused" {
		t.Fatalf("ReactionConfused = %q, want %q", ReactionConfused, "confused")
	}
}
