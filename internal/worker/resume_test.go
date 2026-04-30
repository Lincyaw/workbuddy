package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/supervisor"
	supclient "github.com/Lincyaw/workbuddy/internal/supervisor/client"
)

// fakeSupervisor is a minimal stand-in for the unix-socket supervisor used
// to drive the boot-time resume path in tests. It serves the three
// endpoints the resumer talks to:
//   - GET /agents/:id          → AgentStatus JSON (or 404)
//   - GET /agents/:id/events   → SSE relay of pre-loaded lines respecting
//     from_offset
//
// statusByID and linesByID let each test set the agent's expected status
// transitions and event corpus before starting the server.
type fakeSupervisor struct {
	mu         sync.Mutex
	known      map[string]bool // agentID → exists at all
	statuses   map[string]*supervisor.AgentStatus
	lines      map[string][]string // agentID → 1-indexed event lines
	streamHook map[string]func(w http.ResponseWriter, r *http.Request)
}

func newFakeSupervisor() *fakeSupervisor {
	return &fakeSupervisor{
		known:      map[string]bool{},
		statuses:   map[string]*supervisor.AgentStatus{},
		lines:      map[string][]string{},
		streamHook: map[string]func(w http.ResponseWriter, r *http.Request){},
	}
}

func (f *fakeSupervisor) setStatus(id string, st *supervisor.AgentStatus, lines []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.known[id] = true
	f.statuses[id] = st
	f.lines[id] = lines
}

func (f *fakeSupervisor) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/agents/", func(w http.ResponseWriter, r *http.Request) {
		// Path forms: /agents/:id  or /agents/:id/events
		rest := strings.TrimPrefix(r.URL.Path, "/agents/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		f.mu.Lock()
		known := f.known[id]
		st := f.statuses[id]
		lines := append([]string(nil), f.lines[id]...)
		hook := f.streamHook[id]
		f.mu.Unlock()
		if !known {
			http.Error(w, "agent not found", http.StatusNotFound)
			return
		}
		if len(parts) == 1 {
			// status
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(st)
			return
		}
		if parts[1] != "events" {
			http.NotFound(w, r)
			return
		}
		fromOffset := int64(0)
		if v := r.URL.Query().Get("from_offset"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				http.Error(w, "bad from_offset", http.StatusBadRequest)
				return
			}
			fromOffset = n
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if hook != nil {
			// Custom streaming for tests that need to inject mid-stream
			// behaviour (e.g. simulate a worker restart).
			hook(w, r)
			return
		}
		for i, line := range lines {
			off := int64(i + 1) // offsets are 1-indexed
			if off <= fromOffset {
				continue
			}
			payload, _ := json.Marshal(supclient.StreamEvent{Offset: off, Line: line})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	return mux
}

type recordingHandler struct {
	mu      sync.Mutex
	failed  []store.InFlightTaskForWorker
	failMsg map[string]string
	exited  []store.InFlightTaskForWorker
}

func (r *recordingHandler) OnFailed(_ context.Context, task store.InFlightTaskForWorker, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, task)
	if r.failMsg == nil {
		r.failMsg = map[string]string{}
	}
	r.failMsg[task.TaskID] = reason
	return nil
}

func (r *recordingHandler) OnExited(_ context.Context, task store.InFlightTaskForWorker, _ *supervisor.AgentStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exited = append(r.exited, task)
	return nil
}

func readEventLines(t *testing.T, path string) []supclient.StreamEvent {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	out := []supclient.StreamEvent{}
	for _, raw := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if raw == "" {
			continue
		}
		var ev supclient.StreamEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("decode events line %q: %v", raw, err)
		}
		out = append(out, ev)
	}
	return out
}

// TestResumeInFlight_NormalSSE covers the happy path: a worker boots, finds
// a single in-flight task whose agent is still running, and consumes the
// full SSE stream into events-v1.jsonl. Offsets are strictly monotonic.
func TestResumeInFlight_NormalSSE(t *testing.T) {
	fake := newFakeSupervisor()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	tmp := t.TempDir()
	eventsPath := filepath.Join(tmp, "events-v1.jsonl")
	corpus := []string{
		`{"offset":1,"line":"L1"}`,
		`{"offset":2,"line":"L2"}`,
		`{"offset":3,"line":"L3"}`,
	}
	fake.setStatus("agent-1", &supervisor.AgentStatus{AgentID: "agent-1", Status: "running"}, corpus)
	// streamHook flips the agent to exited after streaming so the
	// post-stream Status probe deterministically sees the terminal state.
	fake.mu.Lock()
	fake.streamHook["agent-1"] = func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		from := int64(0)
		if v := r.URL.Query().Get("from_offset"); v != "" {
			from, _ = strconv.ParseInt(v, 10, 64)
		}
		for i, line := range corpus {
			off := int64(i + 1)
			if off <= from {
				continue
			}
			b, _ := json.Marshal(supclient.StreamEvent{Offset: off, Line: line})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fake.setStatus("agent-1", &supervisor.AgentStatus{AgentID: "agent-1", Status: "exited"}, corpus)
	}
	fake.mu.Unlock()

	client := supclient.NewHTTP(srv.URL, nil)
	rec := &recordingHandler{}

	ResumeInFlightTasks(context.Background(), client, rec, []store.InFlightTaskForWorker{{
		TaskID:            "task-1",
		SupervisorAgentID: "agent-1",
		EventsV1Path:      eventsPath,
	}})

	got := readEventLines(t, eventsPath)
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d (%+v)", len(got), got)
	}
	for i, ev := range got {
		want := int64(i + 1)
		if ev.Offset != want {
			t.Fatalf("event %d offset = %d, want %d", i, ev.Offset, want)
		}
	}
	if len(rec.exited) != 1 || rec.exited[0].TaskID != "task-1" {
		t.Fatalf("expected OnExited for task-1, got %+v", rec.exited)
	}
	if len(rec.failed) != 0 {
		t.Fatalf("did not expect OnFailed, got %+v", rec.failed)
	}
}

// TestResumeInFlight_ResumeAfterRestart simulates a worker SIGKILL midway:
// the events-v1.jsonl already has lines 1-2 on disk when boot starts, and
// the supervisor has lines 1-5 available. The resumer must request
// from_offset=2 and append only lines 3-5 — strictly monotonic, no dups.
func TestResumeInFlight_ResumeAfterRestart(t *testing.T) {
	fake := newFakeSupervisor()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	tmp := t.TempDir()
	eventsPath := filepath.Join(tmp, "events-v1.jsonl")
	// Pre-populate two lines as if the previous worker incarnation had
	// already drained them.
	pre := []supclient.StreamEvent{{Offset: 1, Line: "L1"}, {Offset: 2, Line: "L2"}}
	preBuf := ""
	for _, ev := range pre {
		b, _ := json.Marshal(ev)
		preBuf += string(b) + "\n"
	}
	if err := os.WriteFile(eventsPath, []byte(preBuf), 0o644); err != nil {
		t.Fatalf("seed events file: %v", err)
	}

	corpus := []string{
		`{"offset":1,"line":"L1"}`,
		`{"offset":2,"line":"L2"}`,
		`{"offset":3,"line":"L3"}`,
		`{"offset":4,"line":"L4"}`,
		`{"offset":5,"line":"L5"}`,
	}
	fake.setStatus("agent-2", &supervisor.AgentStatus{AgentID: "agent-2", Status: "running"}, corpus)

	// Verify the supervisor sees from_offset=2 from the resumer.
	var seenOffset int64 = -1
	fake.mu.Lock()
	fake.streamHook["agent-2"] = func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("from_offset"); v != "" {
			n, _ := strconv.ParseInt(v, 10, 64)
			atomic.StoreInt64(&seenOffset, n)
		}
		flusher, _ := w.(http.Flusher)
		from := atomic.LoadInt64(&seenOffset)
		if from < 0 {
			from = 0
		}
		for i, line := range corpus {
			off := int64(i + 1)
			if off <= from {
				continue
			}
			b, _ := json.Marshal(supclient.StreamEvent{Offset: off, Line: line})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	fake.mu.Unlock()

	go func() {
		time.Sleep(50 * time.Millisecond)
		fake.setStatus("agent-2", &supervisor.AgentStatus{AgentID: "agent-2", Status: "exited"}, corpus)
	}()

	rec := &recordingHandler{}
	client := supclient.NewHTTP(srv.URL, nil)

	ResumeInFlightTasks(context.Background(), client, rec, []store.InFlightTaskForWorker{{
		TaskID:            "task-2",
		SupervisorAgentID: "agent-2",
		EventsV1Path:      eventsPath,
	}})

	if got := atomic.LoadInt64(&seenOffset); got != 2 {
		t.Fatalf("supervisor saw from_offset=%d, want 2", got)
	}
	got := readEventLines(t, eventsPath)
	if len(got) != 5 {
		t.Fatalf("want 5 events, got %d (%+v)", len(got), got)
	}
	// Strict monotonic offsets, no duplicates of 1 / 2.
	for i, ev := range got {
		want := int64(i + 1)
		if ev.Offset != want {
			t.Fatalf("event %d offset = %d, want %d", i, ev.Offset, want)
		}
	}
}

// TestResumeInFlight_AgentNotFound covers the 404 branch: the supervisor
// has no record of the agent (e.g. it was reset). The resumer must mark the
// task failed and never attempt the SSE subscription.
func TestResumeInFlight_AgentNotFound(t *testing.T) {
	fake := newFakeSupervisor()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	// Intentionally do not setStatus — the agent is unknown.

	tmp := t.TempDir()
	eventsPath := filepath.Join(tmp, "events-v1.jsonl")

	rec := &recordingHandler{}
	client := supclient.NewHTTP(srv.URL, nil)

	ResumeInFlightTasks(context.Background(), client, rec, []store.InFlightTaskForWorker{{
		TaskID:            "task-3",
		SupervisorAgentID: "ghost",
		EventsV1Path:      eventsPath,
	}})

	if len(rec.failed) != 1 || rec.failed[0].TaskID != "task-3" {
		t.Fatalf("expected OnFailed for task-3, got %+v", rec.failed)
	}
	if !strings.Contains(rec.failMsg["task-3"], "ghost") {
		t.Fatalf("OnFailed reason = %q, want to contain agent id", rec.failMsg["task-3"])
	}
	if len(rec.exited) != 0 {
		t.Fatalf("did not expect OnExited, got %+v", rec.exited)
	}
	if _, err := os.Stat(eventsPath); !os.IsNotExist(err) {
		t.Fatalf("events file should not be created on 404 path: err=%v", err)
	}
}
