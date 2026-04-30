package hooks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// webhookCaptureLimit caps how many response-body bytes the dispatcher
// retains for the invocation timeline. 4 KiB is enough to render a JSON
// error and short-circuit on overly chatty endpoints.
const webhookCaptureLimit = 4096

// httpClientFactory exists so tests can swap in an httptest-backed client.
var httpClientFactory = func() *http.Client { return http.DefaultClient }

// WebhookAction POSTs the v1 payload as application/json and treats any 2xx
// as success.
type WebhookAction struct {
	url     string
	method  string
	headers map[string]string
	client  *http.Client
}

// Type implements Action.
func (w *WebhookAction) Type() string { return ActionTypeWebhook }

// Execute sends the HTTP request and returns an error for non-2xx responses
// or transport-level failures (including context cancellation).
func (w *WebhookAction) Execute(ctx context.Context, ev Event, payload []byte) error {
	return w.Capture(ctx, ev, payload).Err
}

// Capture implements CapturingAction so the dispatcher records the HTTP
// status line + a short response-body preview in the invocation timeline.
// Stdout receives "HTTP <status>\n<body>"; Stderr is unused.
func (w *WebhookAction) Capture(ctx context.Context, _ Event, payload []byte) ActionCapture {
	req, err := http.NewRequestWithContext(ctx, w.method, w.url, bytes.NewReader(payload))
	if err != nil {
		return ActionCapture{Err: fmt.Errorf("hooks: webhook build request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return ActionCapture{Err: fmt.Errorf("hooks: webhook %s: %w", w.url, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, webhookCaptureLimit+1))
	truncated := false
	if len(body) > webhookCaptureLimit {
		body = body[:webhookCaptureLimit]
		truncated = true
	}
	stdout := append([]byte(fmt.Sprintf("HTTP %d %s\n", resp.StatusCode, resp.Status)), body...)
	if truncated {
		stdout = append(stdout, []byte("\n... (truncated)")...)
	}
	out := ActionCapture{Stdout: stdout}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out.Err = fmt.Errorf("hooks: webhook %s: status %d", w.url, resp.StatusCode)
	}
	return out
}

func buildWebhookAction(h *Hook) (Action, []string, error) {
	if strings.TrimSpace(h.Action.URL) == "" {
		return nil, nil, fmt.Errorf("hooks: hook %q: webhook.url is required", h.Name)
	}
	method := strings.ToUpper(strings.TrimSpace(h.Action.Method))
	if method == "" {
		method = http.MethodPost
	}
	if method != http.MethodPost && method != http.MethodPut {
		return nil, nil, fmt.Errorf("hooks: hook %q: webhook.method must be POST or PUT", h.Name)
	}
	resolved, warnings := resolveHeaders(h.Name, h.Action.Headers)
	return &WebhookAction{
		url:     h.Action.URL,
		method:  method,
		headers: resolved,
		client:  httpClientFactory(),
	}, warnings, nil
}

func finalizeWebhookAction(h *Hook) ([]string, error) {
	// Validate URL/method early for fail-fast at LoadConfig time even though
	// the actual instance is only built when the dispatcher binds the action.
	if strings.TrimSpace(h.Action.URL) == "" {
		return nil, fmt.Errorf("hooks: hook %q: webhook.url is required", h.Name)
	}
	method := strings.ToUpper(strings.TrimSpace(h.Action.Method))
	if method != "" && method != http.MethodPost && method != http.MethodPut {
		return nil, fmt.Errorf("hooks: hook %q: webhook.method must be POST or PUT", h.Name)
	}
	_, warnings := resolveHeaders(h.Name, h.Action.Headers)
	return warnings, nil
}

var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// resolveHeaders substitutes ${ENV_VAR} placeholders against the current
// process environment. Missing vars resolve to "" and produce a warning so
// the operator notices at startup.
func resolveHeaders(hookName string, raw map[string]string) (map[string]string, []string) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	var warnings []string
	for k, v := range raw {
		out[k] = envVarPattern.ReplaceAllStringFunc(v, func(match string) string {
			name := match[2 : len(match)-1]
			val, ok := os.LookupEnv(name)
			if !ok || val == "" {
				warnings = append(warnings, fmt.Sprintf("hook %q: header %q references unset env var %s", hookName, k, name))
			}
			return val
		})
	}
	return out, warnings
}
