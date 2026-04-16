package notifier

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
)

const (
	notifierWorkerCount  = 4
	notifierBuffer       = 64
	notifierQueueSize    = 64
	notifierBatchMaxSize = 16
	defaultRetryAttempts = 3
	defaultRetryDelay    = 5 * time.Second
	defaultDedupWindow   = 5 * time.Minute
)

var (
	retryAttempts = defaultRetryAttempts
	retryDelay    = defaultRetryDelay
)

var (
	// nowFn exists for tests.
	nowFn = time.Now

	// httpClient is overridable in tests.
	httpClient = &http.Client{Timeout: 10 * time.Second}
)

type eventLogger interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

type sender interface {
	Send(context.Context, string) error
}

// Option is an optional hook for Notifier construction.
type Option func(*Notifier)

// WithSenders replaces resolved senders.
func WithSenders(s []sender) Option {
	return func(n *Notifier) {
		n.senders = append([]sender(nil), s...)
	}
}

// Notifier consumes AlertEvent stream and delivers events to notification channels.
type Notifier struct {
	enabled       bool
	instanceName  string
	dedupWindow   time.Duration
	batchWindow   time.Duration
	successNotify bool

	bus      *alertbus.Bus
	taskHub  *tasknotify.Hub
	logger   eventLogger
	senders  []sender
	incoming chan alertbus.AlertEvent

	mu              sync.Mutex
	dedup           map[string]time.Time
	failureCounters map[string]int
}

// New creates a notifier bound to the supplied alert bus and task event hub.
func New(cfg config.NotificationsConfig, bus *alertbus.Bus, taskHub *tasknotify.Hub, logger eventLogger, options ...Option) (*Notifier, error) {
	if logger == nil {
		logger = noopEventLogger{}
	}

	n := &Notifier{
		instanceName:    strings.TrimSpace(cfg.InstanceName),
		dedupWindow:     cfg.DedupWindow,
		batchWindow:     cfg.BatchWindow,
		successNotify:   cfg.Success,
		bus:             bus,
		taskHub:         taskHub,
		logger:          logger,
		dedup:           make(map[string]time.Time),
		failureCounters: make(map[string]int),
		incoming:        make(chan alertbus.AlertEvent, notifierQueueSize),
	}
	if n.instanceName == "" {
		n.instanceName = "workbuddy"
	}
	if n.dedupWindow <= 0 {
		n.dedupWindow = defaultDedupWindow
	}
	if n.batchWindow < 0 {
		n.batchWindow = 0
	}

	// Master kill-switch: config only controls whether outbound attempts are
	// attempted. All event collection still happens, but nothing is sent.
	if !cfg.Enabled {
		n.enabled = false
		return n, nil
	}
	n.enabled = true

	var configuredSenders []sender
	if cfg.Slack != nil && cfg.Slack.Enabled {
		s, err := buildWebhookSender("slack", cfg.Slack.WebhookURLEnv)
		if err != nil {
			return nil, err
		}
		configuredSenders = append(configuredSenders, s)
	}
	if cfg.Feishu != nil && cfg.Feishu.Enabled {
		s, err := buildWebhookSender("feishu", cfg.Feishu.WebhookURLEnv)
		if err != nil {
			return nil, err
		}
		configuredSenders = append(configuredSenders, s)
	}
	if cfg.Telegram != nil && cfg.Telegram.Enabled {
		s, err := buildTelegramSender(cfg.Telegram.BotTokenEnv, cfg.Telegram.ChatIDEnv, cfg.Telegram.ParseMode)
		if err != nil {
			return nil, err
		}
		configuredSenders = append(configuredSenders, s)
	}
	if cfg.SMTP != nil && cfg.SMTP.Enabled {
		s, err := buildSMTPSender(cfg.SMTP.HostEnv, cfg.SMTP.PortEnv, cfg.SMTP.UsernameEnv, cfg.SMTP.PasswordEnv, cfg.SMTP.FromEnv, cfg.SMTP.ToEnv)
		if err != nil {
			return nil, err
		}
		configuredSenders = append(configuredSenders, s)
	}
	n.senders = configuredSenders

	for _, option := range options {
		option(n)
	}

	if n.enabled && len(n.senders) == 0 {
		log.Printf("[notifier] notifications enabled but no sender configured")
	}

	return n, nil
}

// Start subscribes notifier workers.
func (n *Notifier) Start(ctx context.Context) {
	if !n.enabled {
		return
	}
	if len(n.senders) == 0 {
		return
	}

	if n.bus != nil {
		n.startBusPump(ctx)
	}
	if n.taskHub != nil {
		n.startTaskPump(ctx)
	}

	if n.batchWindow > 0 {
		go n.runBatchWorker(ctx)
		return
	}
	for i := 0; i < notifierWorkerCount; i++ {
		go n.runWorker(ctx)
	}
}

func (n *Notifier) startBusPump(ctx context.Context) {
	subID, busCh := n.bus.Subscribe()
	go func() {
		defer n.bus.Unsubscribe(subID)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-busCh:
				if !ok {
					return
				}
				n.enqueue(event)
			}
		}
	}()
}

func (n *Notifier) startTaskPump(ctx context.Context) {
	subID, taskCh := n.taskHub.Subscribe()
	go func() {
		defer n.taskHub.Unsubscribe(subID)
		for {
			select {
			case <-ctx.Done():
				return
			case taskEvent, ok := <-taskCh:
				if !ok {
					return
				}
				alert, ok := taskEventToAlertEvent(taskEvent, n.successNotify)
				if !ok {
					continue
				}
				n.maybeEmitRepeatedFailure(alert)
				n.enqueue(alert)
			}
		}
	}()
}

func (n *Notifier) enqueue(event alertbus.AlertEvent) {
	select {
	case n.incoming <- event:
	default:
		log.Printf("[notifier] dropping alert event (queue full): kind=%s repo=%s issue=%d", event.Kind, event.Repo, event.IssueNum)
	}
}

func taskEventToAlertEvent(task tasknotify.TaskEvent, notifySuccess bool) (alertbus.AlertEvent, bool) {
	event := alertbus.AlertEvent{
		Repo:      task.Repo,
		IssueNum:  task.IssueNum,
		AgentName: task.AgentName,
		Timestamp: task.CompletedAt.Unix(),
		Payload: map[string]any{
			"task_id": task.TaskID,
			"status":  task.Status,
		},
	}
	switch task.Status {
	case store.TaskStatusCompleted:
		if task.ExitCode != 0 {
			event.Kind = alertbus.KindAgentExitNonZero
			event.Severity = alertbus.SeverityError
			event.Payload["exit_code"] = task.ExitCode
			return event, true
		}
		if !notifySuccess {
			return alertbus.AlertEvent{}, false
		}
		event.Kind = alertbus.KindTaskCompletedSuccess
		event.Severity = alertbus.SeverityInfo
		event.Payload["exit_code"] = task.ExitCode
		return event, true
	case store.TaskStatusTimeout, store.TaskStatusFailed:
		event.Kind = alertbus.KindAgentExitNonZero
		event.Severity = alertbus.SeverityError
		event.Payload["exit_code"] = task.ExitCode
		return event, true
	default:
		return alertbus.AlertEvent{}, false
	}
}

func (n *Notifier) maybeEmitRepeatedFailure(event alertbus.AlertEvent) {
	if event.Kind != alertbus.KindAgentExitNonZero {
		return
	}
	key := repeatFailureKey(event.Repo, event.IssueNum)

	n.mu.Lock()
	defer n.mu.Unlock()

	n.failureCounters[key]++
	if n.failureCounters[key] <= 1 {
		return
	}

	payload := make(map[string]any, len(event.Payload)+1)
	for k, v := range event.Payload {
		payload[k] = v
	}
	payload["failure_count"] = n.failureCounters[key]
	repeated := alertbus.AlertEvent{
		Kind:      alertbus.KindRepeatedFailure,
		Severity:  alertbus.SeverityWarn,
		Repo:      event.Repo,
		IssueNum:  event.IssueNum,
		AgentName: event.AgentName,
		Timestamp: event.Timestamp,
		Payload:   payload,
	}
	n.enqueue(repeated)
}

func (n *Notifier) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-n.incoming:
			if n.shouldDrop(event) {
				continue
			}
			n.deliver(ctx, formatMessage(event, n.instanceName), event.Repo, event.IssueNum, event.Kind)
		}
	}
}

func (n *Notifier) runBatchWorker(ctx context.Context) {
	var timer *time.Timer
	var batch []alertbus.AlertEvent

	flush := func() {
		if len(batch) == 0 {
			return
		}
		n.deliverBatch(ctx, batch)
		batch = nil
		if timer != nil {
			timer.Stop()
			timer = nil
		}
	}

	for {
		var timerC <-chan time.Time
		if timer != nil {
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			flush()
			return
		case event, ok := <-n.incoming:
			if !ok {
				flush()
				return
			}
			if n.shouldDrop(event) {
				continue
			}
			batch = append(batch, event)
			if len(batch) >= notifierBatchMaxSize {
				flush()
				continue
			}
			if timer == nil {
				timer = time.NewTimer(n.batchWindow)
			}
		case <-timerC:
			flush()
		}
	}
}

func (n *Notifier) deliverBatch(ctx context.Context, events []alertbus.AlertEvent) {
	if len(events) == 0 {
		return
	}
	byIssueKind := make(map[string][]alertbus.AlertEvent)
	for _, ev := range events {
		key := fmt.Sprintf("%s#%d#%s", ev.Repo, ev.IssueNum, ev.Kind)
		byIssueKind[key] = append(byIssueKind[key], ev)
	}
	for _, grouped := range byIssueKind {
		lines := make([]string, 0, len(grouped))
		for _, ev := range grouped {
			lines = append(lines, formatMessage(ev, n.instanceName))
		}
		n.deliver(ctx, strings.Join(lines, "\n"), grouped[0].Repo, grouped[0].IssueNum, "batch")
	}
}

func (n *Notifier) shouldDrop(event alertbus.AlertEvent) bool {
	key := eventDedupKey(event.Repo, event.IssueNum, event.Kind)
	now := nowFn()
	n.mu.Lock()
	defer n.mu.Unlock()

	if exp, ok := n.dedup[key]; ok && now.Before(exp) {
		return true
	}
	n.dedup[key] = now.Add(n.dedupWindow)
	return false
}

func (n *Notifier) deliver(ctx context.Context, message, repo string, issueNum int, kind string) {
	for _, s := range n.senders {
		if err := sendWithRetry(ctx, s, message); err != nil {
			log.Printf("[notifier] sender failed for %s#%d kind=%s after retries: %v", repo, issueNum, kind, err)
			n.logger.Log(eventlog.TypeNotificationFailed, repo, issueNum, map[string]any{
				"event_kind": kind,
				"error":      err.Error(),
			})
		}
	}
}

func sendWithRetry(ctx context.Context, s sender, message string) error {
	wait := retryDelay
	var lastErr error
	for attempt := 1; attempt <= retryAttempts; attempt++ {
		if err := s.Send(ctx, message); err != nil {
			lastErr = err
			if attempt >= retryAttempts {
				return err
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			wait = wait * 2
			continue
		}
		return nil
	}
	return lastErr
}

func eventDedupKey(parts ...interface{}) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, fmt.Sprintf("%v", p))
	}
	return strings.Join(out, "#")
}

func repeatFailureKey(repo string, issueNum int) string {
	return eventDedupKey(repo, issueNum, alertbus.KindAgentExitNonZero)
}

func formatMessage(event alertbus.AlertEvent, instanceName string) string {
	ts := event.Timestamp
	if ts == 0 {
		ts = nowFn().Unix()
	}
	return fmt.Sprintf(
		"severity=%s kind=%s repo=%s issue=%s agent=%s timestamp=%s instance=%s payload=%s",
		event.Severity,
		event.Kind,
		event.Repo,
		issueURL(event.Repo, event.IssueNum),
		event.AgentName,
		time.Unix(ts, 0).UTC().Format(time.RFC3339),
		instanceName,
		marshalPayload(event.Payload),
	)
}

func issueURL(repo string, issueNum int) string {
	if issueNum <= 0 || strings.TrimSpace(repo) == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d", repo, issueNum)
}

func marshalPayload(payload map[string]any) string {
	payload = sanitizePayloadMap(payload)
	data, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func marshalPayloadSafeString(v any) any {
	switch s := v.(type) {
	case string:
		u, err := url.Parse(s)
		if err == nil && strings.TrimSpace(u.Scheme) != "" && strings.TrimSpace(u.Host) != "" {
			return redactURL(s)
		}
	case map[string]any:
		return sanitizePayloadMap(s)
	case []any:
		out := make([]any, 0, len(s))
		for _, item := range s {
			out = append(out, marshalPayloadSafeString(item))
		}
		return out
	}
	return v
}

func sanitizePayloadMap(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	result := make(map[string]any, len(payload))
	for key, value := range payload {
		if strings.Contains(strings.ToLower(key), "webhook") || strings.HasSuffix(strings.ToLower(key), "_url") || strings.EqualFold(key, "url") {
			result[key] = marshalPayloadSafeString(value)
			continue
		}
		result[key] = marshalPayloadSafeString(value)
	}
	return result
}

func buildWebhookSender(channel, envName string) (sender, error) {
	rawURL, ok := os.LookupEnv(envName)
	if !ok || strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("notifier: %s webhook env %q missing", channel, envName)
	}
	rawURL = strings.TrimSpace(rawURL)
	if err := enforceHTTPS(rawURL); err != nil {
		return nil, err
	}

	var bodyFor func(string) string
	switch channel {
	case "slack":
		bodyFor = func(msg string) string {
			data, _ := json.Marshal(map[string]any{"text": msg})
			return string(data)
		}
	case "feishu":
		bodyFor = func(msg string) string {
			data, _ := json.Marshal(map[string]any{
				"msg_type": "text",
				"content": map[string]any{
					"text": msg,
				},
			})
			return string(data)
		}
	default:
		bodyFor = func(msg string) string { return msg }
	}
	log.Printf("[notifier] configured webhook sender for %s (%s)", channel, redactURL(rawURL))
	return &httpSender{endpoint: rawURL, bodyFor: bodyFor, client: httpClient}, nil
}

func buildTelegramSender(tokenEnv, chatIDEnv, parseMode string) (sender, error) {
	token, ok := os.LookupEnv(tokenEnv)
	if !ok || strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("notifier: telegram bot token env %q missing", tokenEnv)
	}
	chatID, ok := os.LookupEnv(chatIDEnv)
	if !ok || strings.TrimSpace(chatID) == "" {
		return nil, fmt.Errorf("notifier: telegram chat id env %q missing", chatIDEnv)
	}
	if _, err := strconv.ParseInt(chatID, 10, 64); err != nil {
		return nil, fmt.Errorf("notifier: telegram chat id must be integer: %w", err)
	}
	return &telegramSender{
		token:     token,
		chatID:    chatID,
		parseMode: strings.TrimSpace(parseMode),
	}, nil
}

func buildSMTPSender(hostEnv, portEnv, usernameEnv, passwordEnv, fromEnv, toEnv string) (sender, error) {
	host, ok := os.LookupEnv(hostEnv)
	if !ok || strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("notifier: smtp host env %q missing", hostEnv)
	}
	portText, ok := os.LookupEnv(portEnv)
	if !ok || strings.TrimSpace(portText) == "" {
		return nil, fmt.Errorf("notifier: smtp port env %q missing", portEnv)
	}
	port, err := strconv.Atoi(strings.TrimSpace(portText))
	if err != nil {
		return nil, fmt.Errorf("notifier: smtp port parse error: %w", err)
	}
	username, ok := os.LookupEnv(usernameEnv)
	if !ok || strings.TrimSpace(username) == "" {
		return nil, fmt.Errorf("notifier: smtp username env %q missing", usernameEnv)
	}
	password, ok := os.LookupEnv(passwordEnv)
	if !ok {
		return nil, fmt.Errorf("notifier: smtp password env %q missing", passwordEnv)
	}
	from, ok := os.LookupEnv(fromEnv)
	if !ok || strings.TrimSpace(from) == "" {
		return nil, fmt.Errorf("notifier: smtp from env %q missing", fromEnv)
	}
	if _, err := mail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("notifier: smtp from is invalid: %w", err)
	}
	to, ok := os.LookupEnv(toEnv)
	if !ok || strings.TrimSpace(to) == "" {
		return nil, fmt.Errorf("notifier: smtp to env %q missing", toEnv)
	}
	if _, err := mail.ParseAddress(to); err != nil {
		return nil, fmt.Errorf("notifier: smtp to is invalid: %w", err)
	}
	return &smtpSender{
		host:     host,
		port:     port,
		username: username,
		password: password,
		from:     from,
		to:       []string{to},
	}, nil
}

func enforceHTTPS(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("notifier: invalid webhook URL: %w", err)
	}
	if strings.TrimSpace(u.Scheme) == "" || u.Scheme != "https" {
		return fmt.Errorf("notifier: webhook URL must be https")
	}
	if strings.TrimSpace(u.Host) == "" {
		return fmt.Errorf("notifier: webhook URL missing host")
	}
	return nil
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid-url)"
	}
	if strings.TrimSpace(u.Host) == "" {
		return "(invalid-host)"
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}

type httpSender struct {
	endpoint string
	bodyFor  func(string) string
	client   *http.Client
}

func (h *httpSender) Send(ctx context.Context, message string) error {
	payload := message
	if h.bodyFor != nil {
		payload = h.bodyFor(message)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, strings.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http notify failed: status=%d", resp.StatusCode)
	}
	return nil
}

type telegramSender struct {
	token     string
	chatID    string
	parseMode string
}

func (t *telegramSender) Send(ctx context.Context, message string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":    t.chatID,
		"text":       message,
		"parse_mode": strings.TrimSpace(t.parseMode),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token),
		strings.NewReader(string(body)),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram notify failed: status=%d", resp.StatusCode)
	}
	return nil
}

type smtpSender struct {
	host     string
	port     int
	username string
	password string
	from     string
	to       []string
}

func (s *smtpSender) Send(_ context.Context, message string) error {
	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	subject := "workbuddy notification"
	messageText := []byte(fmt.Sprintf("To: %s\r\nFrom: %s\r\nSubject: %s\r\n\r\n%s", strings.Join(s.to, ","), s.from, subject, message))
	auth := smtp.PlainAuth("", s.username, s.password, s.host)

	if s.port == 465 {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: s.host})
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		client, err := smtp.NewClient(conn, s.host)
		if err != nil {
			return err
		}
		defer func() { _ = client.Quit() }()
		if err := client.Auth(auth); err != nil {
			return err
		}
		return sendSMTPMail(client, s.from, s.to, messageText)
	}

	client, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := client.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
		return err
	}
	if err := client.Auth(auth); err != nil {
		return err
	}
	return sendSMTPMail(client, s.from, s.to, messageText)
}

func sendSMTPMail(client *smtp.Client, from string, to []string, message []byte) error {
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, addr := range to {
		if err := client.Rcpt(addr); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(message); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

type noopEventLogger struct{}

func (noopEventLogger) Log(string, string, int, interface{}) {}
