package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

const (
	defaultPollTimeout    = 30 * time.Second
	defaultHeartbeat      = 15 * time.Second
	defaultBackoffInitial = time.Second
	defaultBackoffMax     = 60 * time.Second
	defaultRequestTimeout = 35 * time.Second
)

type Config struct {
	BaseURL           string
	WorkerID          string
	Repo              string
	Roles             []string
	Hostname          string
	PollTimeout       time.Duration
	HeartbeatInterval time.Duration
	BackoffInitial    time.Duration
	BackoffMax        time.Duration
	HTTPClient        *http.Client
}

type Client struct {
	baseURL           *url.URL
	httpClient        *http.Client
	workerID          string
	repo              string
	roles             []string
	hostname          string
	pollTimeout       time.Duration
	heartbeatInterval time.Duration
	backoffInitial    time.Duration
	backoffMax        time.Duration
}

type Task struct {
	ID       string               `json:"id"`
	Repo     string               `json:"repo"`
	IssueNum int                  `json:"issue_num"`
	Agent    config.AgentConfig   `json:"agent"`
	Context  launcher.TaskContext `json:"context"`
	Workflow string               `json:"workflow,omitempty"`
	State    string               `json:"state,omitempty"`
	Meta     map[string]string    `json:"meta,omitempty"`
}

type PollResult struct {
	Task *Task `json:"task,omitempty"`
}

type RegisterRequest struct {
	WorkerID string   `json:"worker_id"`
	Repo     string   `json:"repo"`
	Roles    []string `json:"roles"`
	Hostname string   `json:"hostname,omitempty"`
}

type AckRequest struct {
	WorkerID string `json:"worker_id"`
}

type HeartbeatRequest struct {
	WorkerID string `json:"worker_id"`
}

type SubmitResultRequest struct {
	WorkerID string          `json:"worker_id"`
	Result   ExecutionResult `json:"result"`
}

type ExecutionResult struct {
	ExitCode       int                               `json:"exit_code"`
	Stdout         string                            `json:"stdout,omitempty"`
	Stderr         string                            `json:"stderr,omitempty"`
	DurationMS     int64                             `json:"duration_ms"`
	Meta           map[string]string                 `json:"meta,omitempty"`
	SessionPath    string                            `json:"session_path,omitempty"`
	RawSessionPath string                            `json:"raw_session_path,omitempty"`
	LastMessage    string                            `json:"last_message,omitempty"`
	SessionRef     launcher.SessionRef               `json:"session_ref,omitempty"`
	TokenUsage     *launcherevents.TokenUsagePayload `json:"token_usage,omitempty"`
	Error          string                            `json:"error,omitempty"`
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("worker client: base URL is required")
	}
	if strings.TrimSpace(cfg.WorkerID) == "" {
		return nil, fmt.Errorf("worker client: worker ID is required")
	}
	if strings.TrimSpace(cfg.Repo) == "" {
		return nil, fmt.Errorf("worker client: repo is required")
	}

	baseURL, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("worker client: parse base URL: %w", err)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}

	pollTimeout := cfg.PollTimeout
	if pollTimeout <= 0 {
		pollTimeout = defaultPollTimeout
	}
	heartbeatInterval := cfg.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultHeartbeat
	}
	backoffInitial := cfg.BackoffInitial
	if backoffInitial <= 0 {
		backoffInitial = defaultBackoffInitial
	}
	backoffMax := cfg.BackoffMax
	if backoffMax <= 0 {
		backoffMax = defaultBackoffMax
	}
	if backoffMax < backoffInitial {
		backoffMax = backoffInitial
	}

	return &Client{
		baseURL:           baseURL,
		httpClient:        httpClient,
		workerID:          cfg.WorkerID,
		repo:              cfg.Repo,
		roles:             append([]string(nil), cfg.Roles...),
		hostname:          cfg.Hostname,
		pollTimeout:       pollTimeout,
		heartbeatInterval: heartbeatInterval,
		backoffInitial:    backoffInitial,
		backoffMax:        backoffMax,
	}, nil
}

func (c *Client) Register(ctx context.Context) error {
	req := RegisterRequest{
		WorkerID: c.workerID,
		Repo:     c.repo,
		Roles:    append([]string(nil), c.roles...),
		Hostname: c.hostname,
	}
	return c.doJSON(ctx, http.MethodPost, "/api/v1/workers/register", req, nil, http.StatusOK, http.StatusCreated, http.StatusNoContent)
}

func (c *Client) PollTask(ctx context.Context) (*Task, error) {
	values := url.Values{}
	values.Set("worker_id", c.workerID)
	values.Set("repo", c.repo)
	values.Set("timeout", formatSeconds(c.pollTimeout))
	for _, role := range c.roles {
		values.Add("role", role)
	}

	endpoint := "/api/v1/tasks/poll?" + values.Encode()
	var resp PollResult
	status, err := c.doJSONWithStatus(ctx, http.MethodGet, endpoint, nil, &resp, http.StatusOK, http.StatusNoContent)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent || resp.Task == nil {
		return nil, nil
	}
	return resp.Task, nil
}

func (c *Client) Ack(ctx context.Context, taskID string) error {
	return c.doJSON(ctx, http.MethodPost, taskPath(taskID, "ack"), AckRequest{WorkerID: c.workerID}, nil, http.StatusOK, http.StatusCreated, http.StatusNoContent)
}

func (c *Client) Heartbeat(ctx context.Context, taskID string) error {
	return c.doJSON(ctx, http.MethodPost, taskPath(taskID, "heartbeat"), HeartbeatRequest{WorkerID: c.workerID}, nil, http.StatusOK, http.StatusCreated, http.StatusNoContent)
}

func (c *Client) SubmitResult(ctx context.Context, taskID string, result ExecutionResult) error {
	req := SubmitResultRequest{WorkerID: c.workerID, Result: result}
	return c.doJSON(ctx, http.MethodPost, taskPath(taskID, "result"), req, nil, http.StatusOK, http.StatusCreated, http.StatusNoContent)
}

func taskPath(taskID, action string) string {
	return path.Join("/api/v1/tasks", taskID, action)
}

func formatSeconds(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	return fmt.Sprintf("%.3f", d.Seconds())
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, reqBody any, out any, okStatuses ...int) error {
	_, err := c.doJSONWithStatus(ctx, method, endpoint, reqBody, out, okStatuses...)
	return err
}

func (c *Client) doJSONWithStatus(ctx context.Context, method, endpoint string, reqBody any, out any, okStatuses ...int) (int, error) {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return 0, fmt.Errorf("worker client: marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	u := *c.baseURL
	if strings.Contains(endpoint, "?") {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return 0, fmt.Errorf("worker client: parse endpoint %q: %w", endpoint, err)
		}
		u.Path = path.Join(c.baseURL.Path, parsed.Path)
		u.RawQuery = parsed.RawQuery
	} else {
		u.Path = path.Join(c.baseURL.Path, endpoint)
		u.RawQuery = ""
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return 0, fmt.Errorf("worker client: create request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("worker client: %s %s: %w", method, u.String(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	for _, status := range okStatuses {
		if resp.StatusCode == status {
			if out == nil || resp.StatusCode == http.StatusNoContent {
				_, _ = io.Copy(io.Discard, resp.Body)
				return resp.StatusCode, nil
			}
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
				return resp.StatusCode, fmt.Errorf("worker client: decode %s %s: %w", method, u.String(), err)
			}
			return resp.StatusCode, nil
		}
	}

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode, fmt.Errorf("worker client: %s %s: unexpected status %d: %s", method, u.String(), resp.StatusCode, strings.TrimSpace(string(payload)))
}
