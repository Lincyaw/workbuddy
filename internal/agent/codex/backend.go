// Package codex implements the agent.Backend interface via the
// `codex app-server` JSON-RPC protocol.
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

	"github.com/Lincyaw/workbuddy/internal/agent"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

const (
	defaultClientName    = "workbuddy"
	defaultClientVersion = "dev"
)

// Config holds optional configuration for the Codex app-server backend.
type Config struct {
	// Binary overrides the codex binary path (default: "codex").
	Binary string
	// ClientName populates the JSON-RPC initialize handshake.
	ClientName string
	// ClientVersion populates the JSON-RPC initialize handshake.
	ClientVersion string
}

// Backend validates that the codex binary is available and spawns one
// `codex app-server` child per session. This keeps agent-specific environment
// scoping intact while still using the framed JSON-RPC protocol.
type Backend struct {
	cfg Config
}

// NewBackend verifies the codex binary is present.
func NewBackend(cfg Config) (*Backend, error) {
	bin := cfg.Binary
	if bin == "" {
		bin = "codex"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("codex: binary %q not found: %w", bin, err)
	}
	if cfg.ClientName == "" {
		cfg.ClientName = defaultClientName
	}
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = defaultClientVersion
	}
	cfg.Binary = bin
	return &Backend{cfg: cfg}, nil
}

func (b *Backend) NewSession(ctx context.Context, spec agent.Spec) (agent.Session, error) {
	cmdArgs := []string{}
	// This is a top-level Codex CLI flag, not an app-server subcommand flag.
	if spec.Sandbox == "danger-full-access" {
		cmdArgs = append(cmdArgs, "--dangerously-bypass-approvals-and-sandbox")
	}
	cmdArgs = append(cmdArgs, "app-server", "--listen", "stdio://")
	cmd := exec.CommandContext(ctx, b.cfg.Binary, cmdArgs...)
	if spec.Workdir != "" {
		cmd.Dir = spec.Workdir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex: start app-server: %w", err)
	}

	sess := &session{
		cfg:        b.cfg,
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		events:     make(chan agent.Event, 256),
		done:       make(chan struct{}),
		procDone:   make(chan error, 1),
		pending:    make(map[string]chan Response),
		spec:       spec,
		start:      time.Now(),
		sessionRef: agent.SessionRef{Kind: "codex-thread"},
	}
	go sess.readLoop()
	go sess.captureStderr()

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sess.initialize(initCtx); err != nil {
		_ = sess.Close()
		return nil, err
	}
	if err := sess.startThread(initCtx); err != nil {
		_ = sess.Close()
		return nil, err
	}
	if err := sess.startTurn(initCtx); err != nil {
		_ = sess.Close()
		return nil, err
	}

	return sess, nil
}

func (b *Backend) Shutdown(_ context.Context) error { return nil }

type session struct {
	cfg Config

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  atomic.Int64
	pending map[string]chan Response
	closed  bool

	events chan agent.Event
	done   chan struct{}
	start  time.Time
	spec   agent.Spec

	threadID string
	turnID   string

	exitCode     int
	duration     time.Duration
	waitErr      error
	finalMsg     string
	filesChanged map[string]struct{}
	lastError    string
	sessionRef   agent.SessionRef

	finishOnce sync.Once
	stopOnce   sync.Once
	procDone   chan error
}

func (s *session) ID() string {
	if s.threadID != "" {
		return s.threadID
	}
	return s.sessionRef.ID
}

func (s *session) Events() <-chan agent.Event { return s.events }

func (s *session) Wait(ctx context.Context) (agent.Result, error) {
	select {
	case <-s.done:
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	files := make([]string, 0, len(s.filesChanged))
	for path := range s.filesChanged {
		files = append(files, path)
	}
	return agent.Result{
		ExitCode:     s.exitCode,
		FinalMsg:     s.finalMsg,
		FilesChanged: files,
		Duration:     s.duration,
		SessionRef:   s.sessionRef,
	}, s.waitErr
}

func (s *session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	threadID := s.threadID
	turnID := s.turnID
	s.mu.Unlock()
	if threadID == "" || turnID == "" {
		return nil
	}
	_, err := s.call(ctx, "turn/interrupt", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	})
	return err
}

func (s *session) Close() error {
	s.finish("interrupted", context.Canceled)
	s.shutdownProcess()

	select {
	case <-s.procDone:
		return nil
	case <-time.After(2 * time.Second):
		if s.cmd.Process != nil {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	}

	select {
	case <-s.procDone:
	case <-time.After(2 * time.Second):
		return errors.New("codex: app-server did not exit after SIGKILL")
	}
	return nil
}

func (s *session) shutdownProcess() {
	s.stopOnce.Do(func() {
		_ = s.stdin.Close()
		if s.cmd.Process != nil {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
		}
	})
}

func (s *session) initialize(ctx context.Context) error {
	_, err := s.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    s.cfg.ClientName,
			"version": s.cfg.ClientVersion,
		},
	})
	if err != nil {
		return fmt.Errorf("codex: initialize: %w", err)
	}
	if err := s.notify("initialized", nil); err != nil {
		return fmt.Errorf("codex: initialized: %w", err)
	}
	return nil
}

func (s *session) startThread(ctx context.Context) error {
	params := map[string]any{
		"cwd": s.spec.Workdir,
	}
	if s.spec.Model != "" {
		params["model"] = s.spec.Model
	}
	// App-server does not inherit the top-level dangerous bypass flag into the
	// thread sandbox policy. Even in danger mode we must still set the thread
	// sandbox explicitly or Codex falls back to a read-only sandbox.
	if s.spec.Sandbox != "" {
		params["sandbox"] = s.spec.Sandbox
	}
	if s.spec.Approval != "" {
		params["approvalPolicy"] = s.spec.Approval
	}

	result, err := s.call(ctx, "thread/start", params)
	if err != nil {
		return fmt.Errorf("codex: thread/start: %w", err)
	}
	var payload struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return fmt.Errorf("codex: parse thread/start response: %w", err)
	}
	if payload.Thread.ID == "" {
		return fmt.Errorf("codex: thread/start returned empty thread id")
	}

	s.mu.Lock()
	s.threadID = payload.Thread.ID
	s.sessionRef.ID = payload.Thread.ID
	s.mu.Unlock()
	return nil
}

func (s *session) startTurn(ctx context.Context) error {
	params := map[string]any{
		"threadId": s.threadID,
		"input": []map[string]any{{
			"type": "text",
			"text": s.spec.Prompt,
		}},
	}
	if s.spec.Model != "" {
		params["model"] = s.spec.Model
	}
	if s.spec.Approval != "" {
		params["approvalPolicy"] = s.spec.Approval
	}

	result, err := s.call(ctx, "turn/start", params)
	if err != nil {
		return fmt.Errorf("codex: turn/start: %w", err)
	}
	var payload struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return fmt.Errorf("codex: parse turn/start response: %w", err)
	}
	if payload.Turn.ID != "" {
		s.mu.Lock()
		s.turnID = payload.Turn.ID
		s.mu.Unlock()
	}
	return nil
}

func (s *session) captureStderr() {
	if s.stderr == nil {
		return
	}
	scanner := bufio.NewScanner(s.stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		s.emit(newEvent("log", s.currentTurnID(), launcherevents.LogPayload{
			Stream: "stderr",
			Line:   line,
		}, nil))
	}
	_, _ = io.Copy(io.Discard, s.stderr)
}

func (s *session) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID.Add(1)
	req := Request{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("codex: marshal %s request: %w", method, err)
	}
	data = append(data, '\n')

	key := requestIDForInt(id)
	ch := make(chan Response, 1)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("codex: session already closed")
	}
	s.pending[key] = ch
	s.mu.Unlock()

	s.writeMu.Lock()
	_, err = s.stdin.Write(data)
	s.writeMu.Unlock()
	if err != nil {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		return nil, fmt.Errorf("codex: write %s request: %w", method, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		select {
		case resp := <-ch:
			if resp.Error != nil {
				return nil, resp.Error
			}
			return resp.Result, nil
		default:
		}
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-s.done:
		select {
		case resp := <-ch:
			if resp.Error != nil {
				return nil, resp.Error
			}
			return resp.Result, nil
		default:
		}
		return nil, errors.New("codex: session closed")
	}
}

func (s *session) notify(method string, params any) error {
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(data)
	return err
}

func (s *session) reply(id json.RawMessage, payload any) error {
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(append([]byte(nil), id...)),
		"result":  payload,
	})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(data)
	return err
}

func (s *session) replyError(id json.RawMessage, code int, message string) error {
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(data)
	return err
}

func (s *session) readLoop() {
	scanner := bufio.NewScanner(s.stdout)
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
			s.mu.Lock()
			ch := s.pending[key]
			delete(s.pending, key)
			s.mu.Unlock()
			if ch != nil {
				ch <- resp
				close(ch)
			}
		case len(envelope.ID) > 0 && envelope.Method != "":
			var req ServerRequest
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}
			s.handleServerRequest(req)
		case envelope.Method != "":
			var notif Notification
			if err := json.Unmarshal(line, &notif); err != nil {
				continue
			}
			s.handleNotification(notif, json.RawMessage(line))
		}
	}

	waitErr := s.cmd.Wait()
	s.procDone <- waitErr
	close(s.events)
	if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
		s.finish("failed", waitErr)
	} else {
		s.finish("completed", nil)
	}
}

func (s *session) handleServerRequest(req ServerRequest) {
	switch req.Method {
	case "item/commandExecution/requestApproval":
		_ = s.reply(req.ID, map[string]any{"decision": "acceptForSession"})
	case "item/fileChange/requestApproval":
		_ = s.reply(req.ID, map[string]any{"decision": "acceptForSession"})
	case "item/permissions/requestApproval":
		var params struct {
			Permissions any `json:"permissions"`
		}
		_ = json.Unmarshal(req.Params, &params)
		_ = s.reply(req.ID, map[string]any{
			"permissions": params.Permissions,
			"scope":       "session",
		})
	case "execCommandApproval", "applyPatchApproval":
		_ = s.reply(req.ID, map[string]any{"decision": "approved_for_session"})
	case "item/tool/requestUserInput":
		_ = s.reply(req.ID, map[string]any{"answers": map[string]any{}})
	case "mcpServer/elicitation/request":
		_ = s.reply(req.ID, map[string]any{"action": "decline"})
	case "item/tool/call":
		_ = s.reply(req.ID, map[string]any{
			"success": false,
			"contentItems": []map[string]any{{
				"type": "inputText",
				"text": "workbuddy does not expose client-side dynamic tools",
			}},
		})
	default:
		_ = s.replyError(req.ID, -32601, fmt.Sprintf("unsupported server request %q", req.Method))
	}
}

func (s *session) handleNotification(notif Notification, raw json.RawMessage) {
	for _, evt := range mapNotification(notif.Method, notif.Params, raw) {
		s.emit(evt)
	}
	s.observeNotification(notif.Method, notif.Params)
}

func (s *session) observeNotification(method string, params json.RawMessage) {
	switch method {
	case "turn/started":
		var payload struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(params, &payload); err == nil && payload.Turn.ID != "" {
			s.mu.Lock()
			s.turnID = payload.Turn.ID
			s.mu.Unlock()
		}
	case "item/completed":
		var payload struct {
			Item map[string]json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return
		}
		itemType := rawString(payload.Item, "type")
		switch itemType {
		case "agentMessage":
			if text := rawString(payload.Item, "text"); text != "" {
				phase := rawString(payload.Item, "phase")
				if phase == "final_answer" || phase == "" {
					s.mu.Lock()
					s.finalMsg = text
					s.mu.Unlock()
				}
			}
		case "fileChange":
			for _, change := range rawPatchChanges(payload.Item) {
				s.trackChangedFile(change.Path)
			}
		}
	case "turn/completed":
		var payload struct {
			Turn struct {
				ID         string `json:"id"`
				Status     string `json:"status"`
				DurationMS int64  `json:"durationMs"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return
		}
		status := payload.Turn.Status
		if status == "" {
			status = "completed"
		}
		if payload.Turn.ID != "" {
			s.mu.Lock()
			s.turnID = payload.Turn.ID
			s.mu.Unlock()
		}
		var waitErr error
		exitCode := 0
		switch status {
		case "completed":
			exitCode = 0
		case "interrupted":
			exitCode = 130
			waitErr = context.Canceled
		default:
			exitCode = 1
			s.mu.Lock()
			msg := s.lastError
			s.mu.Unlock()
			if msg == "" {
				msg = "codex turn failed"
			}
			waitErr = errors.New(msg)
		}
		s.finishWithDuration(status, exitCode, waitErr, payload.Turn.DurationMS)
		go s.shutdownProcess()
	case "error":
		var payload struct {
			Error struct {
				Message string          `json:"message"`
				Code    json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
		}
		if err := json.Unmarshal(params, &payload); err == nil {
			s.mu.Lock()
			s.lastError = payload.Error.Message
			s.mu.Unlock()
		}
	}
}

func (s *session) emit(evt agent.Event) {
	select {
	case s.events <- evt:
	case <-s.done:
	}
}

func (s *session) currentTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.turnID != "":
		return s.turnID
	case s.threadID != "":
		return s.threadID
	default:
		return s.sessionRef.ID
	}
}

func (s *session) trackChangedFile(path string) {
	if path == "" {
		return
	}
	s.mu.Lock()
	if s.filesChanged == nil {
		s.filesChanged = make(map[string]struct{})
	}
	s.filesChanged[path] = struct{}{}
	s.mu.Unlock()
}

func (s *session) finish(status string, err error) {
	durationMS := int64(time.Since(s.start) / time.Millisecond)
	s.finishWithDuration(status, exitCodeForStatus(status), err, durationMS)
}

func (s *session) finishWithDuration(_ string, exitCode int, err error, durationMS int64) {
	s.finishOnce.Do(func() {
		s.mu.Lock()
		s.exitCode = exitCode
		s.waitErr = err
		if durationMS > 0 {
			s.duration = time.Duration(durationMS) * time.Millisecond
		} else {
			s.duration = time.Since(s.start)
		}
		s.closed = true
		s.mu.Unlock()
		close(s.done)
	})
}

func exitCodeForStatus(status string) int {
	switch status {
	case "completed":
		return 0
	case "interrupted":
		return 130
	default:
		return 1
	}
}
