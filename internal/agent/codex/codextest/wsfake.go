// Package codextest provides an in-process WebSocket fake of the codex
// `app-server` JSON-RPC interface for unit tests. It replaces the
// pre-REQ-127 python stdio fake (writeFakeCodexBinary).
//
// Tests get a URL string + cleanup func. Set WORKBUDDY_CODEX_URL or pass
// the URL into codex.Config.URL so the backend dials the fake.
package codextest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Mode controls how the fake replies to turn/start. Mirrors the modes
// used by the legacy python stdio fake so existing test assertions
// translate one-for-one.
type Mode string

const (
	// ModeComplete responds with a single agentMessage("OK") then
	// turn/completed. Default.
	ModeComplete Mode = "complete"
	// ModeStderrAsLog emits a process-scoped notification (configWarning)
	// before turn completion to exercise the worker's "log fan-out"
	// path that replaces the prior stderr capture in the stdio model.
	ModeStderrAsLog Mode = "stderr-as-log"
	// ModeInterrupt stalls after turn/started so the test can call
	// turn/interrupt and verify the interrupted-completion path.
	ModeInterrupt Mode = "interrupt"
	// ModeFlood emits 500 paired item events to test backpressure /
	// event-drop on a slow consumer.
	ModeFlood Mode = "flood"
	// ModeContentEcho echoes the first text input back as the agent
	// message — useful for prompt-routing tests.
	ModeContentEcho Mode = "content-echo"
)

// Config configures the fake server.
type Config struct {
	// Mode drives turn/start replies. Defaults to ModeComplete.
	Mode Mode
	// LogPath, if non-empty, is appended with one JSON object per
	// observed JSON-RPC message (matches the stdio fake's log format
	// so existing test assertions can read it back).
	LogPath string
	// ThreadID overrides the thread id returned from thread/start
	// (default "thread-test"). Subsequent threads get a numeric
	// suffix (-2, -3, ...).
	ThreadID string
	// AdditionalThreadResponses lets tests inject extra fields into
	// the thread/start result (e.g. modelProvider). Merged on top of
	// the canonical reply.
	AdditionalThreadResponses map[string]any
}

// Server is a running fake. Close is registered with t.Cleanup
// automatically; URL is the ws:// endpoint to dial.
type Server struct {
	URL    string
	close  func()
	addr   net.Addr
	calls  atomic.Int64
	logMu  sync.Mutex
	logF   *os.File
	cfg    Config
	thread atomic.Int64
}

// NewServer starts an in-process fake on a random loopback port. The
// shutdown is registered with t.Cleanup, so callers do not need to
// defer s.Close() — but explicit Close() is supported for tests that
// want to tear the server down before the surrounding test exits.
func NewServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.Mode == "" {
		cfg.Mode = ModeComplete
	}
	if cfg.ThreadID == "" {
		cfg.ThreadID = "thread-test"
	}
	srv := &Server{cfg: cfg}
	if cfg.LogPath != "" {
		f, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("codextest: open log %s: %v", cfg.LogPath, err)
		}
		srv.logF = f
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleWS)
	httpSrv := &http.Server{Handler: mux}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("codextest: listen: %v", err)
	}
	srv.addr = ln.Addr()
	srv.URL = "ws://" + ln.Addr().String()

	go func() { _ = httpSrv.Serve(ln) }()

	var closeOnce sync.Once
	srv.close = func() {
		closeOnce.Do(func() {
			_ = httpSrv.Shutdown(context.Background())
			_ = ln.Close()
			if srv.logF != nil {
				_ = srv.logF.Close()
			}
		})
	}
	t.Cleanup(srv.close)
	return srv
}

// Close shuts the fake server.
func (s *Server) Close() { s.close() }

// CallCount returns the number of JSON-RPC messages observed so far.
func (s *Server) CallCount() int64 { return s.calls.Load() }

func (s *Server) logf(obj any) {
	if s.logF == nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	data, _ := json.Marshal(obj)
	_, _ = s.logF.Write(data)
	_, _ = s.logF.Write([]byte{'\n'})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Allow tests to drive without browser-style origin checks.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusInternalError, "fake exiting")
	conn.SetReadLimit(64 * 1024 * 1024)

	ctx := r.Context()
	for {
		_, frame, err := conn.Read(ctx)
		if err != nil {
			return
		}
		s.calls.Add(1)
		var msg map[string]any
		if jerr := json.Unmarshal(frame, &msg); jerr != nil {
			continue
		}
		s.logf(msg)
		method, _ := msg["method"].(string)
		switch method {
		case "initialize":
			s.reply(ctx, conn, msg["id"], map[string]any{
				"userAgent":      "codextest-fake",
				"codexHome":      "/tmp",
				"platformFamily": "unix",
				"platformOs":     "linux",
			})
		case "initialized":
			// no reply
		case "thread/start":
			n := s.thread.Add(1)
			tid := s.cfg.ThreadID
			if n > 1 {
				tid = fmt.Sprintf("%s-%d", s.cfg.ThreadID, n)
			}
			result := map[string]any{
				"thread":            map[string]any{"id": tid},
				"model":             "gpt-5.4-mini",
				"modelProvider":     "openai",
				"approvalPolicy":    paramOrDefault(msg, "approvalPolicy", "never"),
				"approvalsReviewer": "user",
				"sandbox":           map[string]any{"type": "workspaceWrite"},
				"cwd":               paramOrDefault(msg, "cwd", ""),
			}
			for k, v := range s.cfg.AdditionalThreadResponses {
				result[k] = v
			}
			s.reply(ctx, conn, msg["id"], result)
		case "turn/start":
			s.handleTurnStart(ctx, conn, msg)
		case "turn/interrupt":
			tid := paramString(msg, "threadId", s.cfg.ThreadID)
			s.reply(ctx, conn, msg["id"], map[string]any{})
			s.notify(ctx, conn, "turn/completed", map[string]any{
				"threadId": tid,
				"turn":     map[string]any{"id": "turn-test", "items": []any{}, "status": "interrupted", "durationMs": 7},
			})
		case "thread/archive":
			s.reply(ctx, conn, msg["id"], map[string]any{})
		default:
			// Echo back an empty result for unknown calls if they
			// have an id. Notifications drop silently.
			if id, ok := msg["id"]; ok {
				s.reply(ctx, conn, id, map[string]any{})
			}
		}
	}
}

func (s *Server) handleTurnStart(ctx context.Context, conn *websocket.Conn, msg map[string]any) {
	tid := paramString(msg, "threadId", s.cfg.ThreadID)
	prompt := extractPrompt(msg)
	finalText := "OK"
	if s.cfg.Mode == ModeContentEcho {
		finalText = prompt
	} else if strings.Contains(prompt, "HELLO") {
		finalText = "HELLO"
	} else if strings.Contains(prompt, "PONG") {
		finalText = "PONG"
	}

	s.reply(ctx, conn, msg["id"], map[string]any{
		"turn": map[string]any{"id": "turn-test", "items": []any{}, "status": "inProgress"},
	})
	s.notify(ctx, conn, "turn/started", map[string]any{
		"threadId": tid,
		"turn":     map[string]any{"id": "turn-test", "items": []any{}, "status": "inProgress"},
	})

	switch s.cfg.Mode {
	case ModeFlood:
		for i := 0; i < 500; i++ {
			s.notify(ctx, conn, "item/started", map[string]any{
				"threadId": tid, "turnId": "turn-test",
				"item": map[string]any{"type": "agentMessage", "id": fmt.Sprintf("msg-%d", i), "text": "", "phase": "final_answer"},
			})
			s.notify(ctx, conn, "item/completed", map[string]any{
				"threadId": tid, "turnId": "turn-test",
				"item": map[string]any{"type": "agentMessage", "id": fmt.Sprintf("msg-%d", i), "text": "chunk", "phase": "final_answer"},
			})
		}
		s.notify(ctx, conn, "turn/completed", map[string]any{
			"threadId": tid,
			"turn":     map[string]any{"id": "turn-test", "items": []any{}, "status": "completed", "durationMs": 5},
		})
		return
	case ModeInterrupt:
		// Don't emit further events; wait for turn/interrupt.
		return
	case ModeStderrAsLog:
		// Emit a process-scoped notification (configWarning) that the
		// new readLoop fans out as a log event to active sessions.
		s.notify(ctx, conn, "configWarning", map[string]any{"text": "rpc warning over WS"})
		fallthrough
	case ModeComplete, ModeContentEcho:
		s.notify(ctx, conn, "item/started", map[string]any{
			"threadId": tid, "turnId": "turn-test",
			"item": map[string]any{"type": "agentMessage", "id": "msg-1", "text": "", "phase": "final_answer"},
		})
		s.notify(ctx, conn, "item/agentMessage/delta", map[string]any{
			"threadId": tid, "turnId": "turn-test", "itemId": "msg-1", "delta": finalText,
		})
		s.notify(ctx, conn, "item/completed", map[string]any{
			"threadId": tid, "turnId": "turn-test",
			"item": map[string]any{"type": "agentMessage", "id": "msg-1", "text": finalText, "phase": "final_answer"},
		})
		s.notify(ctx, conn, "thread/tokenUsage/updated", map[string]any{
			"threadId": tid, "turnId": "turn-test",
			"tokenUsage": map[string]any{
				"total": map[string]any{"inputTokens": 10, "outputTokens": 2, "cachedInputTokens": 1, "totalTokens": 12, "reasoningOutputTokens": 0},
				"last":  map[string]any{"inputTokens": 10, "outputTokens": 2, "cachedInputTokens": 1, "totalTokens": 12, "reasoningOutputTokens": 0},
			},
		})
		s.notify(ctx, conn, "turn/completed", map[string]any{
			"threadId": tid,
			"turn":     map[string]any{"id": "turn-test", "items": []any{}, "status": "completed", "durationMs": 15},
		})
	}
}

// reply writes a JSON-RPC response for id with result. Errors are
// silenced because tests don't drive shutdown through the writer path.
func (s *Server) reply(ctx context.Context, conn *websocket.Conn, id any, result any) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	data, _ := json.Marshal(resp)
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(wctx, websocket.MessageText, data)
}

func (s *Server) notify(ctx context.Context, conn *websocket.Conn, method string, params any) {
	notif := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	data, _ := json.Marshal(notif)
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(wctx, websocket.MessageText, data)
}

func paramOrDefault(msg map[string]any, key, def string) string {
	params, ok := msg["params"].(map[string]any)
	if !ok {
		return def
	}
	if v, ok := params[key].(string); ok && v != "" {
		return v
	}
	return def
}

func paramString(msg map[string]any, key, def string) string {
	return paramOrDefault(msg, key, def)
}

func extractPrompt(msg map[string]any) string {
	params, ok := msg["params"].(map[string]any)
	if !ok {
		return ""
	}
	input, ok := params["input"].([]any)
	if !ok {
		return ""
	}
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := item["type"].(string); t == "text" {
			if text, _ := item["text"].(string); text != "" {
				return text
			}
		}
	}
	return ""
}
