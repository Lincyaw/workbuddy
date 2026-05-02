// Package codex — shared app-server WebSocket client.
//
// A single `codex app-server --listen ws://HOST:PORT` child process is
// supervised by `workbuddy supervisor` (see cmd/supervisor_codex_sidecar.go)
// and dialed by the worker over a WebSocket connection. Each agent
// session multiplexes onto one JSON-RPC "thread" on that shared
// connection. This replaces the earlier stdio-pipe model in which the
// worker fork+exec'd codex itself; lifting codex out of the worker's
// process tree is what makes worker redeploy non-destructive to the
// codex runtime (REQ-127).
//
// Codex 0.125.0's `--listen unix://PATH` is advertised in --help but
// rejected at runtime, so WebSocket is the only non-stdio transport
// supported. Bind must be loopback because codex's WS server has no
// authentication.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

// envCodexURL overrides Config.URL. Set via the worker systemd unit so
// operators can swap endpoints without rebuilding.
const envCodexURL = "WORKBUDDY_CODEX_URL"

// defaultCodexURL matches defaultCodexSidecarListen() in cmd/supervisor_codex_sidecar.go.
// The two constants are intentionally duplicated rather than imported across
// packages to keep cmd/ and internal/agent/codex/ decoupled.
const defaultCodexURL = "ws://127.0.0.1:7177"

// resolveCodexURL returns the WebSocket URL the appServer dials, in
// precedence order: explicit cfg.URL, $WORKBUDDY_CODEX_URL, the default.
func resolveCodexURL(cfg Config) string {
	if u := strings.TrimSpace(cfg.URL); u != "" {
		return u
	}
	if u := strings.TrimSpace(os.Getenv(envCodexURL)); u != "" {
		return u
	}
	return defaultCodexURL
}

// appServer owns the single shared WebSocket connection to the supervised
// `codex app-server` sidecar. It multiplexes JSON-RPC traffic for many
// concurrent sessions (threads). Safe for concurrent use from multiple
// goroutines.
//
// Lifecycle is one-shot: ensureConnected dials lazily on first use; if the
// connection drops (codex restarted, network glitch, supervisor cycled),
// the appServer transitions to a permanent failed state and all subsequent
// calls fail with deadErr. The next worker restart re-creates the Backend
// from scratch and re-dials. v1 deliberately keeps reconnect out of scope;
// the fail-fast model matches the prior stdio-pipe behaviour.
type appServer struct {
	cfg Config
	url string

	mu       sync.Mutex
	conn     *websocket.Conn
	pending  map[string]chan Response
	threads  map[string]*session
	nextID   atomic.Int64
	started  bool
	closed   bool
	initErr  error
	deadErr  error
	doneOnce sync.Once

	// connCtx scopes the read loop. shutdown cancels it to make any
	// in-flight Conn.Read return immediately.
	connCtx    context.Context
	connCancel context.CancelFunc
	readDone   chan struct{}
}

// newAppServer creates an idle manager. The connection is dialed on the
// first call to ensureConnected.
func newAppServer(cfg Config) *appServer {
	return &appServer{
		cfg:      cfg,
		url:      resolveCodexURL(cfg),
		pending:  make(map[string]chan Response),
		threads:  make(map[string]*session),
		readDone: make(chan struct{}),
	}
}

// ensureConnected dials the supervised codex sidecar if not already
// connected. Subsequent calls return nil until the connection drops.
func (a *appServer) ensureConnected(ctx context.Context) error {
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

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, a.url, &websocket.DialOptions{
		// codex frames its messages well below the default limit but
		// raise the read limit defensively — long agent messages can
		// exceed the 32KB default.
	})
	if err != nil {
		return fmt.Errorf("codex: dial %s: %w (is the supervised codex sidecar running? supervisor must be started with --codex-binary)", a.url, err)
	}
	// 8 MB per frame: codex agentMessage frames stay well below this in
	// practice, but we keep a buffer for tool-result frames carrying long
	// command outputs. Loopback-only bind limits the blast radius if a
	// malformed frame slipped through, but capping protects the worker
	// from a single huge frame OOMing it.
	conn.SetReadLimit(8 * 1024 * 1024)

	a.mu.Lock()
	a.conn = conn
	a.connCtx, a.connCancel = context.WithCancel(context.Background())
	a.started = true
	a.mu.Unlock()

	go a.readLoop()

	initCtx, cancelInit := context.WithTimeout(ctx, 10*time.Second)
	defer cancelInit()
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

// writeFrame serializes one JSON-RPC message as a WebSocket text frame.
// coder/websocket.Conn.Write is safe for concurrent use.
func (a *appServer) writeFrame(ctx context.Context, payload []byte) error {
	a.mu.Lock()
	conn := a.conn
	closed := a.closed
	a.mu.Unlock()
	if closed || conn == nil {
		return errors.New("codex: shared app-server connection closed")
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}

// call issues a JSON-RPC request and waits for its response. Multiple
// goroutines may call concurrently; coder/websocket serializes frame
// writes internally.
func (a *appServer) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := a.nextID.Add(1)
	req := Request{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("codex: marshal %s request: %w", method, err)
	}

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

	if err := a.writeFrame(ctx, data); err != nil {
		a.mu.Lock()
		delete(a.pending, key)
		a.mu.Unlock()
		return nil, fmt.Errorf("codex: write %s request: %w", method, err)
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

// writeFireAndForget marshals envelope as JSON and ships one WS frame
// with a fixed 5s write deadline. Used by notify/reply/replyError where
// no response is expected.
func (a *appServer) writeFireAndForget(envelope any) error {
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.writeFrame(ctx, data)
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
	return a.writeFireAndForget(req)
}

func (a *appServer) reply(id json.RawMessage, payload any) error {
	return a.writeFireAndForget(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  payload,
	})
}

func (a *appServer) replyError(id json.RawMessage, code int, message string) error {
	return a.writeFireAndForget(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

// readLoop consumes WebSocket text frames and routes responses /
// notifications / server requests to the correct destination (pending
// response channel or session).
func (a *appServer) readLoop() {
	defer close(a.readDone)
	for {
		a.mu.Lock()
		conn := a.conn
		a.mu.Unlock()
		if conn == nil {
			return
		}
		_, frame, err := conn.Read(a.connCtx)
		if err != nil {
			// Either ctx cancel (shutdown) or the codex side closed
			// the WS — both routed through onConnectionClosed which
			// fails pending calls and tears down active sessions.
			a.onConnectionClosed(err)
			return
		}

		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if jerr := json.Unmarshal(frame, &envelope); jerr != nil {
			continue
		}
		switch {
		case len(envelope.ID) > 0 && envelope.Method == "":
			var resp Response
			if jerr := json.Unmarshal(frame, &resp); jerr != nil {
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
			if jerr := json.Unmarshal(frame, &req); jerr != nil {
				continue
			}
			a.dispatchServerRequest(req)
		case envelope.Method != "":
			var notif Notification
			if jerr := json.Unmarshal(frame, &notif); jerr != nil {
				continue
			}
			a.dispatchNotification(notif, append(json.RawMessage(nil), frame...))
		}
	}
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
	// threadId. Fan process-scoped log notifications out to every active
	// session so operators still see them, mirroring the prior stderr-
	// capture path. Other notifications drop silently.
	if isProcessScopedLogNotif(notif.Method) {
		payload := launcherevents.LogPayload{Stream: "codex", Line: notif.Method}
		for _, s := range a.activeSessions() {
			s.emit(newEvent("log", s.currentTurnID(), payload, nil))
		}
	}
}

func isProcessScopedLogNotif(method string) bool {
	switch method {
	case "configWarning", "mcpServer/startupStatus":
		return true
	}
	return false
}

// onConnectionClosed marks the connection dead, fails all pending calls,
// and forces every active session to finish.
func (a *appServer) onConnectionClosed(closeErr error) {
	a.doneOnce.Do(func() {
		a.mu.Lock()
		if closeErr == nil {
			closeErr = errors.New("codex: shared app-server connection closed")
		}
		a.deadErr = closeErr
		pending := a.pending
		a.pending = make(map[string]chan Response)
		threads := a.threads
		a.threads = make(map[string]*session)
		a.mu.Unlock()
		for _, ch := range pending {
			select {
			case ch <- Response{Error: &RPCError{Code: -32000, Message: closeErr.Error()}}:
			default:
			}
			close(ch)
		}
		for _, s := range threads {
			s.finishWithDuration("failed", 1, fmt.Errorf("codex: shared app-server connection closed: %w", closeErr), 0)
			s.closeEvents()
		}
	})
}

// shutdown closes the WebSocket connection and waits for the read loop
// to exit. Safe to call multiple times.
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
	a.mu.Lock()
	conn := a.conn
	cancel := a.connCancel
	a.mu.Unlock()

	if cancel != nil {
		cancel() // make any in-flight Conn.Read return
	}
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "worker shutting down")
	}
	select {
	case <-a.readDone:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("codex: read loop did not exit within shutdown deadline")
	}
}
