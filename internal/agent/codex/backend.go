// Package codex implements the agent.Backend interface via the
// `codex mcp-server` JSON-RPC protocol.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/google/uuid"
)

// Config holds optional configuration for the Codex MCP backend.
type Config struct {
	// Binary overrides the codex binary path (default: "codex").
	Binary string
}

// Backend manages a single `codex mcp-server` child process and
// multiplexes sessions (threads) over it.
type Backend struct {
	cfg    Config
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu       sync.Mutex
	nextID   atomic.Int64
	pending  map[int64]chan Response   // request ID -> response channel
	threads  map[string]chan agent.Event // thread ID -> event channel
	closed   bool
	done     chan struct{}
}

// NewBackend starts `codex mcp-server` and returns a Backend.
func NewBackend(cfg Config) (*Backend, error) {
	bin := cfg.Binary
	if bin == "" {
		bin = "codex"
	}

	// Check if codex binary is available.
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("codex: binary %q not found: %w", bin, err)
	}

	cmd := exec.Command(bin, "mcp-server")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex: start mcp-server: %w", err)
	}

	b := &Backend{
		cfg:     cfg,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		pending: make(map[int64]chan Response),
		threads: make(map[string]chan agent.Event),
		done:    make(chan struct{}),
	}
	go b.readLoop()
	return b, nil
}

// readLoop reads newline-delimited JSON from stdout, routing responses
// by ID and notifications by thread.
func (b *Backend) readLoop() {
	defer close(b.done)
	scanner := bufio.NewScanner(b.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()

		// Try to determine if this is a response (has "id") or notification (has "method", no "id").
		var probe struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}

		if probe.ID != nil {
			// It's a response.
			var resp Response
			if err := json.Unmarshal(line, &resp); err != nil {
				continue
			}
			b.mu.Lock()
			ch, ok := b.pending[resp.ID]
			if ok {
				delete(b.pending, resp.ID)
			}
			b.mu.Unlock()
			if ok {
				ch <- resp
				close(ch)
			}
		} else if probe.Method != "" {
			// It's a notification.
			var notif Notification
			if err := json.Unmarshal(line, &notif); err != nil {
				continue
			}
			evt := mapNotification(notif.Method, notif.Params)

			// Try to extract thread_id from params to route to the right session.
			threadID := extractThreadID(notif.Params)
			b.mu.Lock()
			ch, ok := b.threads[threadID]
			b.mu.Unlock()
			if ok {
				select {
				case ch <- evt:
				default:
					// Drop if full; non-blocking.
				}
			}
		}
	}
}

// extractThreadID attempts to read a "thread_id" field from params JSON.
func extractThreadID(params json.RawMessage) string {
	var p struct {
		ThreadID string `json:"thread_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	return p.ThreadID
}

// call sends a JSON-RPC request and waits for the response.
func (b *Backend) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	id := b.nextID.Add(1)
	req := Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("codex: marshal request: %w", err)
	}
	data = append(data, '\n')

	ch := make(chan Response, 1)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, fmt.Errorf("codex: backend is shut down")
	}
	b.pending[id] = ch
	b.mu.Unlock()

	if _, err := b.stdin.Write(data); err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, fmt.Errorf("codex: write request: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, ctx.Err()
	case <-b.done:
		return nil, fmt.Errorf("codex: mcp-server exited")
	}
}

func (b *Backend) NewSession(ctx context.Context, spec agent.Spec) (agent.Session, error) {
	id := uuid.New().String()

	// Start a thread.
	type threadStartParams struct {
		Workdir string `json:"workdir,omitempty"`
	}
	result, err := b.call(ctx, "thread/start", threadStartParams{Workdir: spec.Workdir})
	if err != nil {
		return nil, fmt.Errorf("codex: thread/start: %w", err)
	}

	var threadResult struct {
		ThreadID string `json:"thread_id"`
	}
	if err := json.Unmarshal(result, &threadResult); err != nil {
		return nil, fmt.Errorf("codex: parse thread/start result: %w", err)
	}
	threadID := threadResult.ThreadID
	if threadID == "" {
		threadID = id
	}

	events := make(chan agent.Event, 64)

	b.mu.Lock()
	b.threads[threadID] = events
	b.mu.Unlock()

	s := &session{
		id:       id,
		threadID: threadID,
		backend:  b,
		events:   events,
		done:     make(chan struct{}),
		start:    time.Now(),
	}

	// Start a turn with the prompt.
	type turnStartParams struct {
		ThreadID string `json:"thread_id"`
		Prompt   string `json:"prompt"`
		Model    string `json:"model,omitempty"`
	}
	go func() {
		defer close(s.done)
		_, turnErr := b.call(ctx, "turn/start", turnStartParams{
			ThreadID: threadID,
			Prompt:   spec.Prompt,
			Model:    spec.Model,
		})
		s.mu.Lock()
		s.duration = time.Since(s.start)
		if turnErr != nil {
			s.waitErr = turnErr
			s.exitCode = 1
		}
		s.mu.Unlock()
	}()

	return s, nil
}

func (b *Backend) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()

	_ = b.stdin.Close()

	grace := 5 * time.Second
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-b.done:
		return b.cmd.Wait()
	case <-timer.C:
		if b.cmd.Process != nil {
			_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
		}
	case <-ctx.Done():
		if b.cmd.Process != nil {
			_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
		}
		return ctx.Err()
	}

	select {
	case <-b.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.cmd.Wait()
}

type session struct {
	id       string
	threadID string
	backend  *Backend
	events   chan agent.Event
	done     chan struct{}
	start    time.Time

	mu       sync.Mutex
	exitCode int
	finalMsg string
	duration time.Duration
	waitErr  error
}

func (s *session) ID() string                { return s.id }
func (s *session) Events() <-chan agent.Event { return s.events }

func (s *session) Wait(ctx context.Context) (agent.Result, error) {
	select {
	case <-s.done:
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return agent.Result{
		ExitCode: s.exitCode,
		FinalMsg: s.finalMsg,
		Duration: s.duration,
	}, s.waitErr
}

func (s *session) Interrupt(ctx context.Context) error {
	type interruptParams struct {
		ThreadID string `json:"thread_id"`
	}
	_, err := s.backend.call(ctx, "turn/interrupt", interruptParams{ThreadID: s.threadID})
	return err
}

func (s *session) Close() error {
	s.backend.mu.Lock()
	delete(s.backend.threads, s.threadID)
	s.backend.mu.Unlock()
	return nil
}
