// Package codex implements the agent.Backend interface via the
// `codex app-server` JSON-RPC protocol over WebSocket.
//
// The codex app-server child is supervised externally by `workbuddy
// supervisor` (see cmd/supervisor_codex_sidecar.go) so it survives worker
// redeploy / upgrade — REQ-127's process-lifecycle abstraction. The
// worker dials a long-lived WebSocket and multiplexes JSON-RPC threads
// onto it; per-agent cwd/model/sandbox/approval policy still flows
// through thread/start params. Per-agent env isolation is still not
// available (the supervisor's environment applies to the whole codex
// process), matching the 2026-04-22 design decision.
//
// Codex's own `--dangerously-bypass-approvals-and-sandbox` flag is set
// once on the supervisor side via `workbuddy supervisor --codex-bypass-
// approvals-and-sandbox`; the worker no longer toggles it per-session
// because it does not own the codex process.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
)

const (
	defaultClientName    = "workbuddy"
	defaultClientVersion = "dev"
	// Give the read loop a brief chance to flush process-scoped log
	// notifications that were emitted just before turn completion, so
	// session consumers do not miss trailing log events due to close timing.
	sessionCloseGracePeriod = 25 * time.Millisecond
)

// Config holds optional configuration for the Codex app-server backend.
type Config struct {
	// URL is the codex app-server WebSocket endpoint (default
	// ws://127.0.0.1:7177; matches the supervisor sidecar default).
	// Honours $WORKBUDDY_CODEX_URL when empty.
	URL string
	// ClientName populates the JSON-RPC initialize handshake.
	ClientName string
	// ClientVersion populates the JSON-RPC initialize handshake.
	ClientVersion string
}

// Backend is the worker-level codex agent.Backend. It owns a single shared
// WebSocket connection to the supervised codex app-server and starts one
// thread per session.
type Backend struct {
	cfg Config

	mu       sync.Mutex
	server   *appServer
	serverMu sync.Mutex // serialize ensureConnected racing against Shutdown
}

// NewBackend prepares a Backend; the WebSocket is dialed lazily on the
// first NewSession.
func NewBackend(cfg Config) (*Backend, error) {
	if cfg.ClientName == "" {
		cfg.ClientName = defaultClientName
	}
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = defaultClientVersion
	}
	return &Backend{cfg: cfg}, nil
}

// sharedServer returns the shared app-server connection manager,
// creating it lazily on first call. With WebSocket transport the
// supervisor controls all CLI flags (including --dangerously-bypass-
// approvals-and-sandbox), so the worker no longer has to negotiate them
// per session.
func (b *Backend) sharedServer() *appServer {
	b.serverMu.Lock()
	defer b.serverMu.Unlock()
	if b.server == nil {
		b.server = newAppServer(b.cfg)
	}
	return b.server
}

func (b *Backend) NewSession(ctx context.Context, spec agent.Spec) (agent.Session, error) {
	srv := b.sharedServer()
	if err := srv.ensureConnected(ctx); err != nil {
		return nil, err
	}

	sess := &session{
		server:     srv,
		events:     make(chan agent.Event, 256),
		done:       make(chan struct{}),
		spec:       spec,
		start:      time.Now(),
		sessionRef: agent.SessionRef{Kind: "codex-thread"},
	}

	startCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := sess.startThread(startCtx); err != nil {
		sess.finishWithDuration("failed", 1, err, 0)
		sess.closeEvents()
		return nil, err
	}
	if err := sess.startTurn(startCtx); err != nil {
		// Thread was started but we failed to kick off a turn: archive it
		// best-effort so the server can reclaim resources.
		sess.archiveBestEffort()
		if sess.threadID != "" {
			sess.server.unregisterSession(sess.threadID)
		}
		sess.finishWithDuration("failed", 1, err, 0)
		sess.closeEvents()
		return nil, err
	}
	return sess, nil
}

// Shutdown tears down the shared app-server process. Safe to call multiple
// times.
func (b *Backend) Shutdown(ctx context.Context) error {
	b.serverMu.Lock()
	srv := b.server
	b.serverMu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.shutdown(ctx)
}

// session is a handle for one thread on the shared app-server. It does NOT
// own the child process.
type session struct {
	server *appServer

	mu     sync.Mutex
	events chan agent.Event
	done   chan struct{}
	start  time.Time
	spec   agent.Spec
	closed bool

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
	closeOnce  sync.Once

	// droppedEvents counts notifications that emit() had to drop because the
	// events channel was full. Incremented atomically by the readLoop caller.
	droppedEvents int64
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
	_, err := s.server.call(ctx, "turn/interrupt", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	})
	return err
}

// Close ends the session. It does NOT kill the shared app-server. If a turn
// is still active we best-effort interrupt it, then archive the thread so
// the server can release thread-scoped resources.
func (s *session) Close() error {
	// Best-effort interrupt using a short-lived context. Ignore errors;
	// finish() below will mark the session done regardless.
	interruptCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.Interrupt(interruptCtx)
	cancel()

	s.finish("interrupted", context.Canceled)
	s.archiveBestEffort()
	if s.threadID != "" {
		s.server.unregisterSession(s.threadID)
	}
	s.closeEvents()
	return nil
}

func (s *session) archiveBestEffort() {
	s.mu.Lock()
	threadID := s.threadID
	s.mu.Unlock()
	if threadID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = s.server.call(ctx, "thread/archive", map[string]any{"threadId": threadID})
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
	// Per-agent env (spec.Env) is intentionally not forwarded here. The
	// app-server protocol has no documented per-thread env-injection hook,
	// and tools spawned by codex inherit the shared process environment.
	// See decisions.md 2026-04-22 for why this deployment does not require
	// per-agent env isolation.

	result, err := s.server.call(ctx, "thread/start", params)
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
	s.server.registerSession(payload.Thread.ID, s)
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

	result, err := s.server.call(ctx, "turn/start", params)
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

// handleServerRequest handles server-initiated requests scoped to this
// thread. Currently we apply blanket-approval for command and file changes
// (matching the single-process behavior) and decline exotic elicitations /
// dynamic-tool calls.
func (s *session) handleServerRequest(req ServerRequest) {
	handleServerRequestWithWriter(req, s.server)
}

// rpcReplier is the minimal interface handleServerRequestWithWriter needs to
// answer a server request. Both *appServer and *session satisfy it.
type rpcReplier interface {
	reply(id json.RawMessage, payload any) error
	replyError(id json.RawMessage, code int, message string) error
}

func handleServerRequestWithWriter(req ServerRequest, w rpcReplier) {
	switch req.Method {
	case "item/commandExecution/requestApproval":
		_ = w.reply(req.ID, map[string]any{"decision": "acceptForSession"})
	case "item/fileChange/requestApproval":
		_ = w.reply(req.ID, map[string]any{"decision": "acceptForSession"})
	case "item/permissions/requestApproval":
		var params struct {
			Permissions any `json:"permissions"`
		}
		_ = json.Unmarshal(req.Params, &params)
		_ = w.reply(req.ID, map[string]any{
			"permissions": params.Permissions,
			"scope":       "session",
		})
	case "execCommandApproval", "applyPatchApproval":
		_ = w.reply(req.ID, map[string]any{"decision": "approved_for_session"})
	case "item/tool/requestUserInput":
		_ = w.reply(req.ID, map[string]any{"answers": map[string]any{}})
	case "mcpServer/elicitation/request":
		_ = w.reply(req.ID, map[string]any{"action": "decline"})
	case "item/tool/call":
		_ = w.reply(req.ID, map[string]any{
			"success": false,
			"contentItems": []map[string]any{{
				"type": "inputText",
				"text": "workbuddy does not expose client-side dynamic tools",
			}},
		})
	default:
		_ = w.replyError(req.ID, -32601, fmt.Sprintf("unsupported server request %q", req.Method))
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
		// Deregister + archive thread so subsequent sessions reuse the
		// shared server without leaking thread state. Run async so the
		// read loop isn't blocked waiting for thread/archive to round-trip.
		go func() {
			s.archiveBestEffort()
			if s.threadID != "" {
				s.server.unregisterSession(s.threadID)
			}
			time.Sleep(sessionCloseGracePeriod)
			s.closeEvents()
		}()
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

// emit must not block: it is called inline from the shared app-server
// readLoop, and blocking here halts notification routing for every other
// session on the same process. Drops are counted in droppedEvents. Safe to
// race with finishWithDuration's close — a send-on-closed-channel panic is
// recovered.
func (s *session) emit(evt agent.Event) {
	defer func() { _ = recover() }()
	select {
	case s.events <- evt:
	default:
		atomic.AddInt64(&s.droppedEvents, 1)
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
		if dropped := atomic.LoadInt64(&s.droppedEvents); dropped > 0 {
			log.Printf("codex: session %s: %d events dropped due to slow consumer", s.threadID, dropped)
		}
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

func (s *session) closeEvents() {
	s.closeOnce.Do(func() {
		close(s.events)
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
