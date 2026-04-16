package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
)

type fakeEventLogger struct {
	mu     sync.Mutex
	events []loggedEvent
}

type loggedEvent struct {
	typ     string
	repo    string
	issue   int
	payload interface{}
}

func (l *fakeEventLogger) Log(eventType, repo string, issueNum int, payload interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, loggedEvent{typ: eventType, repo: repo, issue: issueNum, payload: payload})
}

func (l *fakeEventLogger) count(eventType string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := 0
	for _, ev := range l.events {
		if ev.typ == eventType {
			total++
		}
	}
	return total
}

func (l *fakeEventLogger) firstPayload(eventType string) interface{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ev := range l.events {
		if ev.typ == eventType {
			return ev.payload
		}
	}
	return nil
}

type scriptedSender struct {
	mu     sync.Mutex
	calls  int
	delay  time.Duration
	failFn func(attempt int, message string) error
	onSend func(message string)
}

func (s *scriptedSender) Send(_ context.Context, message string) error {
	s.mu.Lock()
	s.calls++
	call := s.calls
	failFn := s.failFn
	onSend := s.onSend
	delay := s.delay
	s.mu.Unlock()

	if onSend != nil {
		onSend(message)
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if failFn == nil {
		return nil
	}
	return failFn(call, message)
}

func (s *scriptedSender) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func waitIntCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timeout waiting for condition")
}

func waitCalls(t *testing.T, sender *scriptedSender, want int, timeout time.Duration) {
	waitIntCondition(t, timeout, func() bool {
		return sender.CallCount() >= want
	})
}

func TestNotifierSlackWebhookReceivesAgentFailureEvent(t *testing.T) {
	payloads := make(chan string, 4)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Logf("read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		payloads <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	oldHTTPClient := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = oldHTTPClient })

	const envName = "WORKBUDDY_TEST_SLACK_WEBHOOK_URL"
	t.Setenv(envName, srv.URL)

	cfg := config.NotificationsConfig{
		Enabled:      true,
		InstanceName: "ci-runner",
		Slack: &config.WebhookChannelConfig{
			Enabled:       true,
			WebhookURLEnv: envName,
		},
	}
	bus := alertbus.NewBus(64)
	taskHub := tasknotify.NewHub()
	notifier, err := New(cfg, bus, taskHub, &fakeEventLogger{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifier.Start(ctx)

	taskHub.Publish(tasknotify.TaskEvent{
		TaskID:      "task-failure-1",
		Repo:        "owner/repo",
		IssueNum:    12,
		AgentName:   "dev-agent",
		Status:      store.TaskStatusCompleted,
		ExitCode:    2,
		CompletedAt: time.Now(),
	})

	select {
	case raw := <-payloads:
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("payload json: %v", err)
		}
		text, ok := payload["text"].(string)
		if !ok {
			t.Fatalf("slack payload text missing: %v", payload)
		}
		if !strings.Contains(text, "kind=agent_exit_non_zero") {
			t.Fatalf("message missing kind: %q", text)
		}
		if !strings.Contains(text, "repo=owner/repo") {
			t.Fatalf("message missing repo: %q", text)
		}
		if !strings.Contains(text, "issue=https://github.com/owner/repo/issues/12") {
			t.Fatalf("message missing issue url: %q", text)
		}
		if !strings.Contains(text, "agent=dev-agent") {
			t.Fatalf("message missing agent: %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for slack webhook call")
	}
}

func TestNotifierDeduplicatesRepeatedFailuresWithinWindow(t *testing.T) {
	retrySender := &scriptedSender{}
	cfg := config.NotificationsConfig{
		Enabled:     true,
		DedupWindow: 20 * time.Millisecond,
	}
	bus := alertbus.NewBus(64)
	notifier, err := New(cfg, bus, tasknotify.NewHub(), &fakeEventLogger{}, WithSenders([]sender{retrySender}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifier.Start(ctx)

	event := alertbus.AlertEvent{
		Repo:      "owner/repo",
		IssueNum:  7,
		Kind:      alertbus.KindAgentExitNonZero,
		Severity:  alertbus.SeverityError,
		Timestamp: time.Now().Unix(),
		AgentName: "dev-agent",
	}

	bus.Publish(event)
	bus.Publish(event)
	waitCalls(t, retrySender, 1, 200*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	bus.Publish(event)
	waitCalls(t, retrySender, 2, 200*time.Millisecond)
}

func TestNotifierRetriesTransientFailureAndEventuallySucceeds(t *testing.T) {
	origAttempts := retryAttempts
	origDelay := retryDelay
	retryAttempts = 3
	retryDelay = 5 * time.Millisecond
	t.Cleanup(func() {
		retryAttempts = origAttempts
		retryDelay = origDelay
	})

	const maxAttempts = 3
	retrySender := &scriptedSender{
		failFn: func(attempt int, _ string) error {
			if attempt >= maxAttempts {
				return nil
			}
			return fmt.Errorf("transient failure")
		},
	}
	cfg := config.NotificationsConfig{Enabled: true}
	bus := alertbus.NewBus(64)
	logger := &fakeEventLogger{}
	notifier, err := New(cfg, bus, tasknotify.NewHub(), logger, WithSenders([]sender{retrySender}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifier.Start(ctx)
	bus.Publish(alertbus.AlertEvent{
		Repo:      "owner/repo",
		IssueNum:  5,
		Kind:      alertbus.KindAgentExitNonZero,
		Severity:  alertbus.SeverityError,
		Timestamp: time.Now().Unix(),
		AgentName: "dev-agent",
	})

	waitCalls(t, retrySender, maxAttempts, 500*time.Millisecond)
	if got := retrySender.CallCount(); got != maxAttempts {
		t.Fatalf("sender calls = %d, want %d", got, maxAttempts)
	}
	if got := logger.count(eventlog.TypeNotificationFailed); got != 0 {
		t.Fatalf("notification failure logs = %d, want 0", got)
	}
}

func TestNotifierLogsFailureAfterRetryExhaustion(t *testing.T) {
	origAttempts := retryAttempts
	origDelay := retryDelay
	retryAttempts = 3
	retryDelay = 5 * time.Millisecond
	t.Cleanup(func() {
		retryAttempts = origAttempts
		retryDelay = origDelay
	})

	retrySender := &scriptedSender{failFn: func(int, string) error { return fmt.Errorf("always fail") }}
	cfg := config.NotificationsConfig{Enabled: true}
	bus := alertbus.NewBus(64)
	logger := &fakeEventLogger{}
	notifier, err := New(cfg, bus, tasknotify.NewHub(), logger, WithSenders([]sender{retrySender}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifier.Start(ctx)
	bus.Publish(alertbus.AlertEvent{
		Repo:      "owner/repo",
		IssueNum:  9,
		Kind:      alertbus.KindAgentExitNonZero,
		Severity:  alertbus.SeverityError,
		Timestamp: time.Now().Unix(),
		AgentName: "agent-a",
	})

	waitCalls(t, retrySender, retryAttempts, 500*time.Millisecond)
	waitIntCondition(t, time.Second, func() bool {
		return logger.count(eventlog.TypeNotificationFailed) > 0
	})
	payload := logger.firstPayload(eventlog.TypeNotificationFailed)
	got, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("unexpected payload type %T", payload)
	}
	if got["event_kind"] != alertbus.KindAgentExitNonZero {
		t.Fatalf("logged payload event_kind=%v, want %q", got["event_kind"], alertbus.KindAgentExitNonZero)
	}
}

func TestNotifierRejectsInsecureWebhookURLs(t *testing.T) {
	cfg := config.NotificationsConfig{
		Enabled: true,
		Slack: &config.WebhookChannelConfig{
			Enabled:       true,
			WebhookURLEnv: "WORKBUDDY_HTTP_WEBHOOK_URL",
		},
	}
	t.Setenv("WORKBUDDY_HTTP_WEBHOOK_URL", "http://hooks.slack.com/services/legacy")

	if _, err := New(cfg, alertbus.NewBus(64), tasknotify.NewHub(), &fakeEventLogger{}); err == nil {
		t.Fatal("expected webhook startup to fail on http scheme")
	}
}

func TestNotifierRedactsWebhookURLInLogs(t *testing.T) {
	buf := &bytes.Buffer{}
	origWriter := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(origWriter) })

	const secretURL = "https://hooks.slack.com/services/T000000/B000/SECRET_TOKEN"
	const envName = "WORKBUDDY_SECRET_SLACK_URL"
	t.Setenv(envName, secretURL)

	cfg := config.NotificationsConfig{
		Enabled: true,
		Slack: &config.WebhookChannelConfig{
			Enabled:       true,
			WebhookURLEnv: envName,
		},
	}
	_, err := New(cfg, nil, tasknotify.NewHub(), &fakeEventLogger{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "https://hooks.slack.com)") {
		t.Fatalf("missing redacted host in log output: %q", out)
	}
	if strings.Contains(out, secretURL) {
		t.Fatalf("full webhook URL leaked in log output")
	}

	msg := formatMessage(alertbus.AlertEvent{
		Severity:  alertbus.SeverityWarn,
		Kind:      alertbus.KindTaskCompletedSuccess,
		Repo:      "owner/repo",
		IssueNum:  55,
		AgentName: "dev-agent",
		Payload:   map[string]any{"webhook_url": secretURL},
		Timestamp: time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC).Unix(),
	}, "instance")
	if strings.Contains(msg, "SECRET_TOKEN") {
		t.Fatalf("full webhook URL-like token leaked in alert message: %q", msg)
	}
}

func TestNotifierMasterKillSwitchDisablesOutboundCalls(t *testing.T) {
	requests := int64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const envName = "WORKBUDDY_KILL_SWITCH_WEBHOOK"
	t.Setenv(envName, srv.URL)
	cfg := config.NotificationsConfig{
		Enabled: false,
		Slack: &config.WebhookChannelConfig{
			Enabled:       true,
			WebhookURLEnv: envName,
		},
	}
	notifier, err := New(cfg, alertbus.NewBus(64), tasknotify.NewHub(), &fakeEventLogger{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifier.Start(ctx)

	notifier.enqueue(alertbus.AlertEvent{
		Repo:      "owner/repo",
		IssueNum:  1,
		Kind:      alertbus.KindAgentExitNonZero,
		Severity:  alertbus.SeverityError,
		AgentName: "dev-agent",
	})
	time.Sleep(50 * time.Millisecond)

	if got := atomic.LoadInt64(&requests); got != 0 {
		t.Fatalf("outbound webhook calls = %d, want 0", got)
	}
}

func TestNotifierDoesNotBlockWhenSenderIsSlow(t *testing.T) {
	const slowDelay = 20 * time.Millisecond
	retrySender := &scriptedSender{delay: slowDelay}
	cfg := config.NotificationsConfig{Enabled: true}
	bus := alertbus.NewBus(64)
	notifier, err := New(cfg, bus, tasknotify.NewHub(), &fakeEventLogger{}, WithSenders([]sender{retrySender}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifier.Start(ctx)

	start := time.Now()
	for i := 0; i < 200; i++ {
		bus.Publish(alertbus.AlertEvent{
			Repo:      "owner/repo",
			IssueNum:  i,
			Kind:      alertbus.KindAgentExitNonZero,
			Severity:  alertbus.SeverityError,
			AgentName: "dev-agent",
			Timestamp: time.Now().Unix(),
		})
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("publish calls blocked too long: %s", elapsed)
	}
	waitCalls(t, retrySender, 1, 500*time.Millisecond)
}

func TestFormatMessageContainsRequiredFields(t *testing.T) {
	timestamp := time.Unix(1713273600, 0).UTC()
	message := formatMessage(alertbus.AlertEvent{
		Severity:  alertbus.SeverityError,
		Kind:      alertbus.KindAgentExitNonZero,
		Repo:      "owner/repo",
		IssueNum:  101,
		AgentName: "dev-agent",
		Timestamp: timestamp.Unix(),
		Payload: map[string]any{
			"task_id": "task-1",
		},
	}, "node-a")

	for _, want := range []string{
		"severity=error",
		"kind=agent_exit_non_zero",
		"repo=owner/repo",
		"issue=https://github.com/owner/repo/issues/101",
		"agent=dev-agent",
		"instance=node-a",
		"timestamp=" + timestamp.Format(time.RFC3339),
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("message missing %q: %q", want, message)
		}
	}
}
