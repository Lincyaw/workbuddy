// Package codex — shared app-server process manager.
//
// A single `codex app-server --listen stdio://` child process is multiplexed
// across all concurrent agent sessions on a worker. Each agent session is a
// JSON-RPC "thread" on that shared process. This replaces the earlier
// one-process-per-session model (see decisions.md 2026-04-20 and superseding
// entry 2026-04-22).
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

// appServer owns the single shared `codex app-server` child process. It
// multiplexes JSON-RPC traffic for many concurrent sessions (threads). It is
// safe for concurrent use from multiple goroutines.
type appServer struct {
	cfg             Config
	dangerousBypass bool

	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	writeMu  sync.Mutex
	pending  map[string]chan Response
	threads  map[string]*session
	nextID   atomic.Int64
	started  bool
	closed   bool
	initErr  error
	procDone chan error
	doneOnce sync.Once
	deadErr  error
}

// newAppServer creates an idle manager. The child process is started on the
// first call to ensureStarted.
func newAppServer(cfg Config, dangerousBypass bool) *appServer {
	return &appServer{
		cfg:             cfg,
		dangerousBypass: dangerousBypass,
		pending:         make(map[string]chan Response),
		threads:         make(map[string]*session),
		procDone:        make(chan error, 1),
	}
}

// ensureStarted spawns and initializes the shared process if it is not
// already running. Subsequent calls return nil until the process dies.
func (a *appServer) ensureStarted(ctx context.Context) error {
	a.mu.Lock()
	if a.started && a.deadErr == nil {
		a.mu.Unlock()
		return nil
	}
	if a.deadErr != nil {
		err := a.deadErr
		a.mu.Unlock()
		return fmt.Errorf("codex: shared app-server unavailable: %w", err)
	}
	if a.initErr != nil {
		err := a.initErr
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()

	args := []string{}
	if a.dangerousBypass {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	args = append(args, "app-server", "--listen", "stdio://")
	// Use Background, not the session ctx: this process outlives any single
	// session. Lifecycle is driven by Shutdown.
	cmd := exec.Command(a.cfg.Binary, args...)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("codex: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("codex: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("codex: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codex: start app-server: %w", err)
	}

	a.mu.Lock()
	a.cmd = cmd
	a.stdin = stdin
	a.stdout = stdout
	a.stderr = stderr
	a.started = true
	a.mu.Unlock()

	go a.readLoop()
	go a.captureStderr()

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.initialize(initCtx); err != nil {
		a.mu.Lock()
		a.initErr = err
		a.mu.Unlock()
		_ = a.shutdownLocked()
		return err
	}
	return nil
}

func (a *appServer) initialize(ctx context.Context) error {
	_, err := a.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    a.cfg.ClientName,
			"version": a.cfg.ClientVersion,
		},
	})
	if err != nil {
		return fmt.Errorf("codex: initialize: %w", err)
	}
	if err := a.notify("initialized", nil); err != nil {
		return fmt.Errorf("codex: initialized: %w", err)
	}
	return nil
}

// registerSession associates a thread id with a session. Call after
// thread/start returns the thread id.
func (a *appServer) registerSession(threadID string, s *session) {
	if threadID == "" {
		return
	}
	a.mu.Lock()
	a.threads[threadID] = s
	a.mu.Unlock()
}

func (a *appServer) unregisterSession(threadID string) {
	if threadID == "" {
		return
	}
	a.mu.Lock()
	delete(a.threads, threadID)
	a.mu.Unlock()
}

func (a *appServer) sessionByThread(threadID string) *session {
	if threadID == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.threads[threadID]
}

func (a *appServer) activeSessions() []*session {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*session, 0, len(a.threads))
	for _, s := range a.threads {
		out = append(out, s)
	}
	return out
}

// call issues a JSON-RPC request and waits for its response. Multiple
// goroutines may call concurrently; writes to stdin are serialized.
func (a *appServer) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := a.nextID.Add(1)
	req := Request{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("codex: marshal %s request: %w", method, err)
	}
	data = append(data, '\n')

	key := requestIDForInt(id)
	ch := make(chan Response, 1)

	a.mu.Lock()
	if a.closed || a.deadErr != nil {
		err := a.deadErr
		a.mu.Unlock()
		if err == nil {
			err = errors.New("codex: shared app-server closed")
		}
		return nil, err
	}
	a.pending[key] = ch
	a.mu.Unlock()

	a.writeMu.Lock()
	_, werr := a.stdin.Write(data)
	a.writeMu.Unlock()
	if werr != nil {
		a.mu.Lock()
		delete(a.pending, key)
		a.mu.Unlock()
		return nil, fmt.Errorf("codex: write %s request: %w", method, werr)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		a.mu.Lock()
		delete(a.pending, key)
		a.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (a *appServer) notify(method string, params any) error {
	req := Notification{JSONRPC: "2.0", Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = raw
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.stdin == nil {
		return errors.New("codex: shared app-server stdin closed")
	}
	_, err = a.stdin.Write(data)
	return err
}

func (a *appServer) reply(id json.RawMessage, payload any) error {
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(append([]byte(nil), id...)),
		"result":  payload,
	})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.stdin == nil {
		return errors.New("codex: shared app-server stdin closed")
	}
	_, err = a.stdin.Write(data)
	return err
}

func (a *appServer) replyError(id json.RawMessage, code int, message string) error {
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(append([]byte(nil), id...)),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.stdin == nil {
		return errors.New("codex: shared app-server stdin closed")
	}
	_, err = a.stdin.Write(data)
	return err
}

// readLoop consumes stdout and routes responses / notifications / server
// requests to the correct destination (pending response channel or session).
func (a *appServer) readLoop() {
	scanner := bufio.NewScanner(a.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			continue
		}
		switch {
		case len(envelope.ID) > 0 && envelope.Method == "":
			var resp Response
			if err := json.Unmarshal(line, &resp); err != nil {
				continue
			}
			key := requestIDKey(resp.ID)
			a.mu.Lock()
			ch := a.pending[key]
			delete(a.pending, key)
			a.mu.Unlock()
			if ch != nil {
				ch <- resp
				close(ch)
			}
		case len(envelope.ID) > 0 && envelope.Method != "":
			var req ServerRequest
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}
			a.dispatchServerRequest(req)
		case envelope.Method != "":
			var notif Notification
			if err := json.Unmarshal(line, &notif); err != nil {
				continue
			}
			a.dispatchNotification(notif, json.RawMessage(line))
		}
	}

	waitErr := a.cmd.Wait()
	a.onProcessExit(waitErr)
}

func (a *appServer) captureStderr() {
	if a.stderr == nil {
		return
	}
	scanner := bufio.NewScanner(a.stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// Stderr is process-scoped, not session-scoped. Fan out to every
		// active session as a log event so operators can still see it
		// alongside per-session artifacts.
		payload := launcherevents.LogPayload{Stream: "stderr", Line: line}
		for _, s := range a.activeSessions() {
			s.emit(newEvent("log", s.currentTurnID(), payload, nil))
		}
	}
	_, _ = io.Copy(io.Discard, a.stderr)
}

// dispatchServerRequest routes a server-initiated request to the owning
// session when the params carry a threadId. When no session can be found
// the request is answered with a safe default reply (preserving the legacy
// blanket-approval behavior of the single-process backend).
func (a *appServer) dispatchServerRequest(req ServerRequest) {
	threadID := extractThreadID(req.Params)
	if sess := a.sessionByThread(threadID); sess != nil {
		sess.handleServerRequest(req)
		return
	}
	handleServerRequestWithWriter(req, a)
}

func (a *appServer) dispatchNotification(notif Notification, raw json.RawMessage) {
	threadID := extractThreadID(notif.Params)
	if sess := a.sessionByThread(threadID); sess != nil {
		sess.handleNotification(notif, raw)
		return
	}
	// Some notifications (e.g. pre-thread errors) may arrive without a
	// threadId; drop silently.
}

// onProcessExit marks the shared process dead, fails all pending calls, and
// forces every active session to finish.
func (a *appServer) onProcessExit(waitErr error) {
	a.doneOnce.Do(func() {
		a.mu.Lock()
		if waitErr == nil {
			waitErr = errors.New("codex: shared app-server exited")
		}
		a.deadErr = waitErr
		pending := a.pending
		a.pending = make(map[string]chan Response)
		threads := a.threads
		a.threads = make(map[string]*session)
		a.mu.Unlock()
		// Fail every in-flight request.
		for _, ch := range pending {
			select {
			case ch <- Response{Error: &RPCError{Code: -32000, Message: waitErr.Error()}}:
			default:
			}
			close(ch)
		}
		// Finish every active session.
		for _, s := range threads {
			s.finishWithDuration("failed", 1, fmt.Errorf("codex: shared app-server exited: %w", waitErr), 0)
			s.closeEvents()
		}
		select {
		case a.procDone <- waitErr:
		default:
		}
	})
}

// shutdown stops the shared process and waits for it to exit.
func (a *appServer) shutdown(ctx context.Context) error {
	a.mu.Lock()
	if !a.started || a.closed {
		a.closed = true
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	a.mu.Unlock()
	return a.shutdownLocked()
}

func (a *appServer) shutdownLocked() error {
	a.writeMu.Lock()
	if a.stdin != nil {
		_ = a.stdin.Close()
	}
	a.writeMu.Unlock()
	if a.cmd != nil && a.cmd.Process != nil {
		_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-a.procDone:
		return nil
	case <-time.After(2 * time.Second):
		if a.cmd != nil && a.cmd.Process != nil {
			_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	select {
	case <-a.procDone:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("codex: shared app-server did not exit after SIGKILL")
	}
}
