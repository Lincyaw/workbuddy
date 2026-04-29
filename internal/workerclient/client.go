package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrUnauthorized = errors.New("workerclient: unauthorized")

const defaultMaxBackoff = 60 * time.Second

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	maxBackoff time.Duration
}

type RegisterRequest struct {
	WorkerID    string   `json:"worker_id"`
	Repo        string   `json:"repo"`
	Roles       []string `json:"roles"`
	Runtime     string   `json:"runtime,omitempty"`
	Repos       []string `json:"repos,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	MgmtBaseURL string   `json:"mgmt_base_url,omitempty"`
}

type Task struct {
	TaskID    string   `json:"task_id"`
	Repo      string   `json:"repo"`
	IssueNum  int      `json:"issue_num"`
	AgentName string   `json:"agent_name"`
	Workflow  string   `json:"workflow,omitempty"`
	State     string   `json:"state,omitempty"`
	Roles     []string `json:"roles,omitempty"`
}

type ResultRequest struct {
	WorkerID      string   `json:"worker_id"`
	Status        string   `json:"status"`
	CurrentLabels []string `json:"current_labels"`
	// InfraFailure marks runs that failed at the launcher layer (exec error,
	// scanner overflow, runtime panic before agent output) rather than
	// returning an agent verdict. Coordinators must NOT translate this into
	// a state-machine failure — the agent never got to decide. See issue
	// #131 / AC-3.
	InfraFailure bool `json:"infra_failure,omitempty"`
	// InfraReason carries a short operator-facing reason for the infra
	// failure (e.g. "exec start error", "scanner buffer overflow"). Only
	// meaningful when InfraFailure is true.
	InfraReason string `json:"infra_reason,omitempty"`
}

type HeartbeatRequest struct {
	WorkerID string `json:"worker_id"`
}

type ReleaseRequest struct {
	WorkerID string `json:"worker_id"`
	Reason   string `json:"reason,omitempty"`
}

func New(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      strings.TrimSpace(token),
		httpClient: httpClient,
		maxBackoff: defaultMaxBackoff,
	}
}

func (c *Client) Register(ctx context.Context, req RegisterRequest) error {
	_, err := c.doJSON(ctx, http.MethodPost, "/api/v1/workers/register", req, nil, http.StatusCreated)
	return err
}

func (c *Client) Unregister(ctx context.Context, workerID string) error {
	_, err := c.doJSON(ctx, http.MethodDelete, "/api/v1/workers/"+url.PathEscape(workerID), nil, nil, http.StatusOK)
	return err
}

func (c *Client) PollTask(ctx context.Context, workerID string, timeout time.Duration) (*Task, error) {
	path := fmt.Sprintf("/api/v1/tasks/poll?worker_id=%s", url.QueryEscape(workerID))
	if timeout > 0 {
		path += "&timeout=" + url.QueryEscape(timeout.String())
	}
	var task Task
	status, err := c.doJSON(ctx, http.MethodGet, path, nil, &task, http.StatusOK, http.StatusNoContent)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &task, nil
}

func (c *Client) SubmitResult(ctx context.Context, taskID string, req ResultRequest) error {
	_, err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/result", req, nil, http.StatusOK)
	return err
}

func (c *Client) Heartbeat(ctx context.Context, taskID string, req HeartbeatRequest) error {
	_, err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/heartbeat", req, nil, http.StatusNoContent)
	return err
}

func (c *Client) ReleaseTask(ctx context.Context, taskID string, req ReleaseRequest) error {
	_, err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/release", req, nil, http.StatusOK, http.StatusNoContent)
	return err
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any, okStatuses ...int) (int, error) {
	backoff := time.Second
	for {
		status, err := c.doJSONOnce(ctx, method, path, body, out, okStatuses...)
		if err == nil {
			return status, nil
		}
		if errors.Is(err, ErrUnauthorized) || !isRetryable(err) || ctx.Err() != nil {
			return 0, err
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > c.maxBackoff {
			backoff = c.maxBackoff
		}
	}
}

func (c *Client) doJSONOnce(ctx context.Context, method, path string, body any, out any, okStatuses ...int) (int, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("workerclient: marshal %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return 0, fmt.Errorf("workerclient: build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("workerclient: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return 0, fmt.Errorf("workerclient: read %s %s: %w", method, path, readErr)
	}

	for _, want := range okStatuses {
		if resp.StatusCode == want {
			if out != nil && len(respBody) > 0 {
				if err := json.Unmarshal(respBody, out); err != nil {
					return 0, fmt.Errorf("workerclient: decode %s %s: %w", method, path, err)
				}
			}
			return resp.StatusCode, nil
		}
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return 0, ErrUnauthorized
	}
	if resp.StatusCode >= 500 {
		return 0, fmt.Errorf("workerclient: server error %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return 0, fmt.Errorf("workerclient: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
}

func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return strings.Contains(err.Error(), "server error")
}
