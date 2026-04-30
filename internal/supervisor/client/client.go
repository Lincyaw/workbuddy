// Package client is the IPC client for the local agent supervisor (issue
// #231 + #234). It speaks HTTP over a unix socket (or any net.Conn dialer
// callers wire in), exposing typed wrappers for the four endpoints the
// supervisor serves: POST /agents, GET /agents/:id, POST /agents/:id/cancel,
// and GET /agents/:id/events (SSE).
//
// The worker uses this client to make subprocess launch + lifecycle
// management stateless w.r.t. its own lifetime: it POSTs to start, persists
// the returned agent_id to coordinator, and resumes SSE on restart. See
// internal/supervisor/supervisor.go for the server side.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/supervisor"
)

// Client talks to a Supervisor over HTTP. The transport may be backed by a
// unix socket (the default in production) or any *http.Client for tests.
type Client struct {
	base string
	hc   *http.Client
}

// NewUnix returns a Client that dials the given unix socket path for every
// request. The socket path should match what was passed to
// supervisor.New(Config{SocketPath: ...}).
func NewUnix(socketPath string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _ /*network*/, _ /*addr*/ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		// SSE: do not multiplex over a connection that may park.
		DisableKeepAlives: false,
	}
	return &Client{
		base: "http://supervisor", // host is irrelevant; transport ignores it
		hc:   &http.Client{Transport: tr},
	}
}

// NewHTTP wraps an arbitrary base URL + http client. Used by tests that wire
// the supervisor.Handler() into httptest.Server.
func NewHTTP(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: hc}
}

// ErrAgentNotFound is returned when the supervisor responds 404 for an agent
// id. Callers (notably the worker resume path) treat this as terminal: the
// task must be marked failed rather than left in-flight.
var ErrAgentNotFound = errors.New("supervisor: agent not found")

// StartAgent posts to /agents and returns the assigned agent id.
func (c *Client) StartAgent(ctx context.Context, req supervisor.StartAgentRequest) (*supervisor.StartAgentResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal start req: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/agents", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build start req: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post /agents: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("post /agents: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out supervisor.StartAgentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode start resp: %w", err)
	}
	return &out, nil
}

// Status returns the current AgentStatus. Returns ErrAgentNotFound when the
// supervisor responds 404 (e.g. the supervisor's state was reset).
func (c *Client) Status(ctx context.Context, agentID string) (*supervisor.AgentStatus, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/agents/"+url.PathEscape(agentID), nil)
	if err != nil {
		return nil, fmt.Errorf("build status req: %w", err)
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("get /agents/%s: %w", agentID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrAgentNotFound
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get /agents/%s: status %d: %s", agentID, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out supervisor.AgentStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}
	return &out, nil
}

// Cancel requests SIGTERM (then SIGKILL after grace). Returns nil when the
// supervisor accepts the cancel (204) and ErrAgentNotFound for 404.
func (c *Client) Cancel(ctx context.Context, agentID string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/agents/"+url.PathEscape(agentID)+"/cancel", nil)
	if err != nil {
		return fmt.Errorf("build cancel req: %w", err)
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return fmt.Errorf("post cancel: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrAgentNotFound
	}
	b, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("post cancel: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

// StreamEvent is one decoded SSE `data:` payload from /agents/:id/events.
type StreamEvent struct {
	Offset int64  `json:"offset"`
	Line   string `json:"line"`
}

// StreamEvents subscribes to the SSE stream and invokes onEvent for every
// stdout line the supervisor delivers, starting at fromOffset (line numbers
// are 1-indexed; pass 0 for "from the beginning"). It returns when the
// supervisor closes the stream (agent exited and drain finished), when ctx
// is cancelled, or when onEvent returns a non-nil error.
//
// On 404 the function returns ErrAgentNotFound — the worker resume path
// distinguishes this from a transient transport error to mark the task
// failed rather than retrying.
func (c *Client) StreamEvents(ctx context.Context, agentID string, fromOffset int64, onEvent func(StreamEvent) error) error {
	q := url.Values{}
	if fromOffset > 0 {
		q.Set("from_offset", strconv.FormatInt(fromOffset, 10))
	}
	target := c.base + "/agents/" + url.PathEscape(agentID) + "/events"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build events req: %w", err)
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return fmt.Errorf("get events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return ErrAgentNotFound
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("get events: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	br := bufio.NewReader(resp.Body)
	// SSE framing: each event terminates with a blank line. We only care
	// about `data:` lines (the supervisor never emits multi-line data
	// payloads). A leading `event: error` is surfaced as an error.
	var dataBuf strings.Builder
	var eventName string
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				// dispatch
				if eventName == "error" {
					return fmt.Errorf("supervisor stream error: %s", dataBuf.String())
				}
				if dataBuf.Len() > 0 {
					var ev StreamEvent
					if jerr := json.Unmarshal([]byte(dataBuf.String()), &ev); jerr != nil {
						return fmt.Errorf("decode SSE data: %w", jerr)
					}
					if cberr := onEvent(ev); cberr != nil {
						return cberr
					}
				}
				dataBuf.Reset()
				eventName = ""
			case strings.HasPrefix(line, ":"):
				// SSE comment / keep-alive
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				if dataBuf.Len() > 0 {
					dataBuf.WriteByte('\n')
				}
				dataBuf.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// flush trailing event without blank-line terminator
				if eventName == "error" {
					return fmt.Errorf("supervisor stream error: %s", dataBuf.String())
				}
				if dataBuf.Len() > 0 {
					var ev StreamEvent
					if jerr := json.Unmarshal([]byte(dataBuf.String()), &ev); jerr == nil {
						_ = onEvent(ev)
					}
				}
				return nil
			}
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return fmt.Errorf("read SSE: %w", err)
		}
	}
}
