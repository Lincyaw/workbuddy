// Package webui provides HTTP handlers for the session viewer web UI.
package webui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// Handler serves the session event API (events.json + SSE stream) consumed
// by the SPA. The legacy HTML list/detail pages have been retired in favour
// of the embedded SPA bundle; see SetFallback for the bridge to it.
type Handler struct {
	sessionsDir string
	basePath    string
	fallback    http.Handler
}

// NewHandler creates a Handler. The store argument is retained for API
// stability; the events.json and stream endpoints read directly from the
// per-session events-v1.jsonl files configured via SetSessionsDir.
func NewHandler(_ *store.Store) *Handler {
	return &Handler{basePath: "/sessions"}
}

// SetSessionsDir configures the directory where per-session event logs live,
// e.g. "<dir>/<session_id>/events-v1.jsonl". Required for the events.json and
// stream endpoints; when unset they return 404.
func (h *Handler) SetSessionsDir(dir string) {
	h.sessionsDir = dir
}

// SetFallback installs a handler used when a request under the session base
// path does not target the events.json or stream endpoint. The SPA scaffold
// uses this to take over the legacy HTML list/detail routes — clients that
// land on /sessions or /sessions/{id} get the SPA shell instead of the
// retired Go-side templates.
//
// When fallback is nil, unrecognised subpaths return 404 (the original
// behaviour minus the deleted list/detail handlers).
func (h *Handler) SetFallback(fallback http.Handler) {
	h.fallback = fallback
}

// Register adds the session API routes (events.json and stream) to the given
// mux. The legacy HTML list and detail pages are no longer registered — the
// SPA handler at "/" owns them.
func (h *Handler) Register(mux *http.ServeMux) {
	h.RegisterAt(mux, h.basePath)
}

// RegisterAt mounts the session API routes under the given base path.
func (h *Handler) RegisterAt(mux *http.ServeMux, basePath string) {
	basePath = strings.TrimRight(basePath, "/")
	if basePath == "" {
		basePath = "/sessions"
	}
	h.basePath = basePath
	mux.HandleFunc(basePath+"/", h.handleSessionSubpath)
}

// handleSessionSubpath dispatches requests under /sessions/ based on suffix:
//
//	/sessions/{id}/events.json    → paginated JSON events
//	/sessions/{id}/stream         → SSE tail
//
// Anything else (including the empty suffix that used to render the HTML
// detail page) is delegated to the SPA fallback when one is configured, or
// returns 404 otherwise.
func (h *Handler) handleSessionSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, h.basePath+"/")
	sessionID, suffix, _ := strings.Cut(rest, "/")
	switch suffix {
	case "events.json":
		markDeprecated(w, r,
			"/sessions/{id}/events.json",
			"/api/v1/sessions/{id}/events")
		h.handleEventsJSON(w, r, sessionID)
	case "stream":
		markDeprecated(w, r,
			"/sessions/{id}/stream",
			"/api/v1/sessions/{id}/stream")
		h.handleStream(w, r, sessionID)
	default:
		// Empty suffix (the legacy detail page) and any other path now
		// belong to the SPA when a fallback is configured.
		if h.fallback != nil {
			h.fallback.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

// HandleAPISessionEvents serves /api/v1/sessions/{id}/events. The new path
// reuses the existing on-disk events-v1.jsonl reader; the legacy
// /sessions/{id}/events.json route is preserved as a 30-day deprecation alias.
func (h *Handler) HandleAPISessionEvents(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := apiSessionID(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.handleEventsJSON(w, r, sessionID)
}

// HandleAPISessionStream serves /api/v1/sessions/{id}/stream (SSE).
func (h *Handler) HandleAPISessionStream(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := apiSessionID(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.handleStream(w, r, sessionID)
}

func apiSessionID(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/api/v1/sessions/")
	id, _, _ := strings.Cut(rest, "/")
	if !isValidSessionID(id) {
		return "", false
	}
	return id, true
}

// markDeprecated tags responses to legacy /sessions/{id}/events.json and
// /sessions/{id}/stream paths. Operators get a `Deprecation: true` header
// (per RFC 8594 the value would be a date, but the issue spec asks for a
// boolean shape) and a stdout log line so log-scrapers can spot lingering
// callers before the alias is removed.
func markDeprecated(w http.ResponseWriter, r *http.Request, oldPath, newPath string) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Sunset", "30 days")
	w.Header().Set("Link", `<`+newPath+`>; rel="successor-version"`)
	log.Printf("[deprecated] old path %s accessed (request %s), prefer %s", oldPath, r.URL.Path, newPath)
}

// handleEventsJSON returns a paginated slice of events from events-v1.jsonl.
//
// Query params:
//
//	offset (int, default 0)       — skip this many events from the start
//	limit  (int, default 200)     — max events returned (capped at 1000)
//	tail   ("1" to take last N)   — when set, returns the last `limit` events;
//	                                `offset` is ignored.
//
// Bulky string fields inside payload/raw are truncated server-side to keep
// the response small. Clients can request the full event via the stream or
// by re-querying without truncation (not yet implemented — intentional).
func (h *Handler) handleEventsJSON(w http.ResponseWriter, r *http.Request, sessionID string) {
	path := h.eventsPath(sessionID)
	if path == "" {
		http.NotFound(w, r)
		return
	}

	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	tail := q.Get("tail") == "1"

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, eventsResponse{Events: []trimmedEvent{}, Total: 0})
			return
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	out, total, start, end, err := readEventSlice(f, offset, limit, tail)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, eventsResponse{
		Events: out,
		Total:  total,
		Start:  start,
		End:    end,
	})
}

// readEventSlice streams the file line-by-line and collects only the requested
// slice so memory stays O(limit) instead of O(total). For `tail` it keeps a
// ring buffer of the last `limit` lines.
func readEventSlice(r io.Reader, offset, limit int, tail bool) ([]trimmedEvent, int, int, int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	if tail {
		ring := make([]string, 0, limit)
		total := 0
		for sc.Scan() {
			line := sc.Text()
			total++
			if len(ring) < limit {
				ring = append(ring, line)
			} else {
				copy(ring, ring[1:])
				ring[limit-1] = line
			}
		}
		if err := sc.Err(); err != nil {
			return nil, 0, 0, 0, err
		}
		start := total - len(ring)
		if start < 0 {
			start = 0
		}
		end := total
		out := make([]trimmedEvent, 0, len(ring))
		for i, line := range ring {
			ev, ok := parseAndTrim(line, start+i)
			if !ok {
				continue
			}
			out = append(out, ev)
		}
		return out, total, start, end, nil
	}

	total := 0
	desiredEnd := offset + limit
	out := make([]trimmedEvent, 0, limit)
	for sc.Scan() {
		idx := total
		total++
		if idx < offset || idx >= desiredEnd {
			continue
		}
		ev, ok := parseAndTrim(sc.Text(), idx)
		if !ok {
			continue
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, 0, err
	}
	start := offset
	if start > total {
		start = total
	}
	end := desiredEnd
	if end > total {
		end = total
	}
	return out, total, start, end, nil
}

// handleStream tails events-v1.jsonl and pushes new events via SSE.
// Polls the file once per second for simplicity (no fsnotify dep).
// Closes cleanly when the client disconnects via r.Context().
func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request, sessionID string) {
	path := h.eventsPath(sessionID)
	if path == "" {
		http.NotFound(w, r)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	afterStr := r.URL.Query().Get("after")
	after, _ := strconv.Atoi(afterStr)
	if after < 0 {
		after = 0
	}

	var f *os.File
	var reader *bufio.Reader
	openFile := func() bool {
		var err error
		f, err = os.Open(path)
		if err != nil {
			return false
		}
		reader = bufio.NewReader(f)
		return true
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	if !openFile() {
		fmt.Fprint(w, ": events file not ready\n\n")
		flusher.Flush()
	}

	idx := 0
	var pending strings.Builder
	emit := func(line string) {
		if idx >= after {
			ev, ok := parseAndTrim(line, idx)
			if ok {
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "event: evt\nid: %d\ndata: %s\n\n", idx, data)
			}
		}
		idx++
	}

	// Initial drain + then poll loop.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		if reader != nil {
			for {
				line, err := reader.ReadString('\n')
				if line != "" {
					pending.WriteString(line)
					if strings.HasSuffix(line, "\n") {
						emit(strings.TrimRight(pending.String(), "\r\n"))
						pending.Reset()
					}
				}
				if err != nil {
					if !errors.Is(err, io.EOF) {
						fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
					}
					break
				}
			}
			flusher.Flush()
		}

		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-ticker.C:
			if reader == nil {
				openFile()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (h *Handler) eventsPath(sessionID string) string {
	if h.sessionsDir == "" || !isValidSessionID(sessionID) {
		return ""
	}
	baseDir := filepath.Clean(h.sessionsDir)
	fullPath := filepath.Join(baseDir, sessionID, "events-v1.jsonl")
	rel, err := filepath.Rel(baseDir, fullPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return fullPath
}

// isValidSessionID rejects empty IDs, dot-paths, and any value containing a
// path separator so a request cannot traverse out of the configured sessions
// directory.
func isValidSessionID(sessionID string) bool {
	if sessionID == "" || sessionID == "." || sessionID == ".." {
		return false
	}
	if strings.ContainsAny(sessionID, `/\`) {
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

type eventsResponse struct {
	Events []trimmedEvent `json:"events"`
	Total  int            `json:"total"`
	Start  int            `json:"start,omitempty"`
	End    int            `json:"end,omitempty"`
}

// trimmedEvent is the on-the-wire shape: a thin projection of
// launcherevents.Event that strips Raw and truncates bulky strings in Payload.
type trimmedEvent struct {
	Index     int    `json:"index"`
	Kind      string `json:"kind"`
	Timestamp string `json:"ts,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	Seq       uint64 `json:"seq"`
	Payload   any    `json:"payload,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

const maxStringLen = 4000

func parseAndTrim(line string, idx int) (trimmedEvent, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return trimmedEvent{}, false
	}
	var raw struct {
		Kind      string          `json:"kind"`
		Timestamp string          `json:"ts"`
		SessionID string          `json:"session_id"`
		TurnID    string          `json:"turn_id"`
		Seq       uint64          `json:"seq"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return trimmedEvent{
			Index: idx,
			Kind:  "unparseable",
			Payload: map[string]string{
				"error": err.Error(),
				"line":  truncString(line, maxStringLen),
			},
			Truncated: len(line) > maxStringLen,
		}, true
	}
	var payload any
	truncated := false
	if len(raw.Payload) > 0 {
		if err := json.Unmarshal(raw.Payload, &payload); err == nil {
			payload, truncated = trimValue(payload)
		} else {
			payload = string(raw.Payload)
		}
	}
	return trimmedEvent{
		Index:     idx,
		Kind:      raw.Kind,
		Timestamp: raw.Timestamp,
		SessionID: raw.SessionID,
		TurnID:    raw.TurnID,
		Seq:       raw.Seq,
		Payload:   payload,
		Truncated: truncated,
	}, true
}

// trimValue recursively walks decoded JSON and truncates long strings in place.
// Returns (possibly-new) value and whether any truncation occurred.
func trimValue(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		if len(x) > maxStringLen {
			return x[:maxStringLen] + "… [truncated]", true
		}
		return x, false
	case map[string]any:
		trunc := false
		for k, val := range x {
			nv, t := trimValue(val)
			x[k] = nv
			trunc = trunc || t
		}
		return x, trunc
	case []any:
		trunc := false
		// Cap array length so a 10k-line tool output doesn't blow the wire.
		const maxArr = 200
		if len(x) > maxArr {
			x = append(x[:maxArr], "… [truncated "+strconv.Itoa(len(x)-maxArr)+" more]")
			trunc = true
		}
		for i, val := range x {
			nv, t := trimValue(val)
			x[i] = nv
			trunc = trunc || t
		}
		return x, trunc
	default:
		return v, false
	}
}

func truncString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "… [truncated]"
}

// SessionURL returns the URL path for a session detail page in the SPA.
func SessionURL(sessionID string) string {
	return fmt.Sprintf("/sessions/%s", sessionID)
}
