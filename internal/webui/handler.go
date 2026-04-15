// Package webui provides HTTP handlers for the session viewer web UI.
package webui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// Handler serves the session viewer web UI.
type Handler struct {
	store        *store.Store
	sessionsDir  string
	basePath     string
	listTmpl     *template.Template
	detailTmpl   *template.Template
	notFoundTmpl *template.Template
}

// NewHandler creates a Handler backed by the given store.
func NewHandler(st *store.Store) *Handler {
	funcMap := template.FuncMap{
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "..."
		},
	}

	h := &Handler{store: st, basePath: "/sessions"}
	h.listTmpl = template.Must(template.New("list").Funcs(funcMap).Parse(listHTML))
	h.detailTmpl = template.Must(template.New("detail").Funcs(funcMap).Parse(detailHTML))
	h.notFoundTmpl = template.Must(template.New("notfound").Parse(notFoundHTML))
	return h
}

// SetSessionsDir configures the directory where per-session event logs live,
// e.g. "<dir>/<session_id>/events-v1.jsonl". Required for the events.json and
// stream endpoints; when unset they return 404.
func (h *Handler) SetSessionsDir(dir string) {
	h.sessionsDir = dir
}

// Register adds the session viewer routes to the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	h.RegisterAt(mux, h.basePath)
}

// RegisterAt mounts the session viewer routes under the given base path.
func (h *Handler) RegisterAt(mux *http.ServeMux, basePath string) {
	basePath = strings.TrimRight(basePath, "/")
	if basePath == "" {
		basePath = "/sessions"
	}
	h.basePath = basePath
	mux.HandleFunc(basePath, h.handleList)
	mux.HandleFunc(basePath+"/", h.handleSessionSubpath)
}

// handleList renders the session list page with optional filtering.
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.SessionFilter{
		Repo:      q.Get("repo"),
		AgentName: q.Get("agent"),
	}
	if issueStr := q.Get("issue"); issueStr != "" {
		if n, err := strconv.Atoi(issueStr); err == nil {
			filter.IssueNum = n
		}
	}

	sessions, err := h.store.ListAgentSessions(filter)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	data := listData{Sessions: sessions, Filter: filter}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.listTmpl.Execute(w, data); err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
	}
}

// handleSessionSubpath dispatches requests under /sessions/ based on suffix:
//
//	/sessions/{id}                → detail HTML
//	/sessions/{id}/events.json    → paginated JSON events
//	/sessions/{id}/stream         → SSE tail
func (h *Handler) handleSessionSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, h.basePath+"/")
	if rest == "" {
		http.Redirect(w, r, h.basePath, http.StatusFound)
		return
	}

	sessionID, suffix, _ := strings.Cut(rest, "/")
	switch suffix {
	case "":
		h.handleDetail(w, r, sessionID)
	case "events.json":
		h.handleEventsJSON(w, r, sessionID)
	case "stream":
		h.handleStream(w, r, sessionID)
	default:
		http.NotFound(w, r)
	}
}

// handleDetail renders a single session detail page.
func (h *Handler) handleDetail(w http.ResponseWriter, r *http.Request, sessionID string) {
	sess, err := h.store.GetAgentSession(sessionID)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if sess == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_ = h.notFoundTmpl.Execute(w, sessionID)
		return
	}

	var taskStatus string
	if sess.TaskID != "" {
		tasks, err := h.store.QueryTasks("")
		if err == nil {
			for _, t := range tasks {
				if t.ID == sess.TaskID {
					taskStatus = t.Status
					break
				}
			}
		}
	}

	hasEvents := false
	if p := h.eventsPath(sessionID); p != "" {
		if _, err := os.Stat(p); err == nil {
			hasEvents = true
		}
	}

	data := detailData{
		Session:    *sess,
		TaskStatus: taskStatus,
		HasEvents:  hasEvents,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.detailTmpl.Execute(w, data); err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
	}
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

// listData is the template data for the session list page.
type listData struct {
	Sessions []store.AgentSession
	Filter   store.SessionFilter
}

// detailData is the template data for the session detail page.
type detailData struct {
	Session    store.AgentSession
	TaskStatus string
	HasEvents  bool
}

// SessionURL returns the URL path for a session detail page.
func SessionURL(sessionID string) string {
	return fmt.Sprintf("/sessions/%s", sessionID)
}
