package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// seedSession writes a session row + an events-v1.jsonl file so the events.json
// and stream paths have something to read.
func seedSession(t *testing.T, st *store.Store, sessionsDir, sessionID string) {
	t.Helper()
	dir := filepath.Join(sessionsDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "events-v1.jsonl"),
		[]byte("{\"kind\":\"a\",\"seq\":1}\n{\"kind\":\"b\",\"seq\":2}\n"),
		0o644,
	); err != nil {
		t.Fatalf("write events: %v", err)
	}
	if _, err := st.CreateSession(store.SessionRecord{
		SessionID: sessionID,
		Repo:      "owner/repo",
		IssueNum:  214,
		AgentName: "dev-agent",
		Status:    store.TaskStatusRunning,
		Dir:       dir,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
}

func TestAPISessionEvents(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	seedSession(t, st, sessionsDir, "session-218")

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		// Mirror the coordinator wiring: dispatch suffix to the right method.
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
		_, suffix, _ := strings.Cut(rest, "/")
		switch suffix {
		case "events":
			h.HandleAPISessionEvents(w, r)
		case "stream":
			h.HandleAPISessionStream(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/v1/sessions/session-218/events?limit=50&offset=0")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), `"kind":"a"`) {
		t.Fatalf("body missing event content: %s", string(body))
	}
}

// TestEventsAPINoTruncation is the #276 regression: server-side per-string
// and per-array truncation broke the SPA's `JSON.parse(payload.line)` because
// payload.line is itself a JSON document. The contract is now: each event in
// the response mirrors the on-disk record byte-for-byte (modulo encoding
// re-formatting), and `truncated` is never true.
func TestEventsAPINoTruncation(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	sessionID := "session-276"
	dir := filepath.Join(sessionsDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Build a payload that previously tripped both guards:
	//   - `line` is a 64 KB JSON string (the kind Claude Code stream-json
	//     emits — itself a JSON document the SPA re-parses).
	//   - `calls` is a 500-element array (>200, the old maxArr).
	innerObj := map[string]any{"text": strings.Repeat("y", 64*1024-200)}
	innerJSON, err := json.Marshal(innerObj)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	if len(innerJSON) <= 4000 {
		t.Fatalf("inner JSON length %d not large enough to trigger old truncation", len(innerJSON))
	}
	calls := make([]map[string]int, 500)
	for i := range calls {
		calls[i] = map[string]int{"i": i}
	}
	payloadObj := map[string]any{
		"line":  string(innerJSON),
		"calls": calls,
	}
	payloadBytes, err := json.Marshal(payloadObj)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	eventLine := append([]byte(`{"kind":"log","seq":1,"payload":`), payloadBytes...)
	eventLine = append(eventLine, '}', '\n')
	if err := os.WriteFile(filepath.Join(dir, "events-v1.jsonl"), eventLine, 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}

	if _, err := st.CreateSession(store.SessionRecord{
		SessionID: sessionID,
		Repo:      "owner/repo",
		IssueNum:  276,
		AgentName: "dev-agent",
		Status:    store.TaskStatusRunning,
		Dir:       dir,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
		_, suffix, _ := strings.Cut(rest, "/")
		switch suffix {
		case "events":
			h.HandleAPISessionEvents(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/v1/sessions/" + sessionID + "/events?limit=10")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var got struct {
		Events []struct {
			Kind      string         `json:"kind"`
			Payload   map[string]any `json:"payload"`
			Truncated bool           `json:"truncated"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Events) != 1 {
		t.Fatalf("events len = %d, want 1", len(got.Events))
	}
	ev := got.Events[0]
	if ev.Truncated {
		t.Fatalf("truncated = true, want false (#276)")
	}

	// 1) `line` must come back at exactly the input length.
	gotLine, ok := ev.Payload["line"].(string)
	if !ok {
		t.Fatalf("payload.line not a string: %T", ev.Payload["line"])
	}
	if len(gotLine) != len(innerJSON) {
		t.Fatalf("payload.line length = %d, want %d", len(gotLine), len(innerJSON))
	}
	// 2) The frontend does `JSON.parse(line)`. Simulate it: the returned
	//    string must still be parseable as JSON.
	var roundtrip map[string]any
	if err := json.Unmarshal([]byte(gotLine), &roundtrip); err != nil {
		t.Fatalf("client-side JSON.parse(line) would fail: %v", err)
	}

	// 3) `calls` must keep all 500 elements (no array cap).
	gotCalls, ok := ev.Payload["calls"].([]any)
	if !ok {
		t.Fatalf("payload.calls not a slice: %T", ev.Payload["calls"])
	}
	if len(gotCalls) != 500 {
		t.Fatalf("payload.calls length = %d, want 500", len(gotCalls))
	}
}
