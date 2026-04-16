package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is the worker-side HTTP client for coordinator task endpoints.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// New creates a worker HTTP client.
func New(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      strings.TrimSpace(token),
		httpClient: httpClient,
	}
}

// PollTask long-polls for the next task.
func (c *Client) PollTask(ctx context.Context, workerID string, timeout time.Duration) (*http.Response, error) {
	values := url.Values{}
	values.Set("worker_id", workerID)
	if timeout > 0 {
		values.Set("timeout", timeout.String())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/tasks/poll?"+values.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("workerclient: build poll request: %w", err)
	}
	c.applyAuth(req)
	return c.httpClient.Do(req)
}

// SubmitResult posts task completion data.
func (c *Client) SubmitResult(ctx context.Context, taskID string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("workerclient: marshal result: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/tasks/"+taskID+"/result", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("workerclient: build result request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyAuth(req)
	return c.httpClient.Do(req)
}

// Heartbeat notifies the coordinator that a task is still alive.
func (c *Client) Heartbeat(ctx context.Context, taskID string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/tasks/"+taskID+"/heartbeat", nil)
	if err != nil {
		return nil, fmt.Errorf("workerclient: build heartbeat request: %w", err)
	}
	c.applyAuth(req)
	return c.httpClient.Do(req)
}

func (c *Client) applyAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
