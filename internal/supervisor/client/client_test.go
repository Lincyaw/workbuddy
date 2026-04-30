package client

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/supervisor"
)

func newTestServer(t *testing.T) (*supervisor.Supervisor, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	s, err := supervisor.New(supervisor.Config{
		SocketPath:  filepath.Join(dir, "sup.sock"),
		StateDir:    dir,
		CancelGrace: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = s.Close()
	})
	return s, srv
}

// TestStartAndStream covers REQ-061: a normal /agents POST returns an
// agent_id and the SSE stream replays every stdout line with monotonic
// offsets, terminating cleanly when the subprocess exits.
func TestStartAndStream(t *testing.T) {
	_, srv := newTestServer(t)
	c := NewHTTP(srv.URL, srv.Client())

	resp, err := c.StartAgent(context.Background(), supervisor.StartAgentRequest{
		Runtime: "/bin/sh",
		Args:    []string{"-c", "printf 'one\\ntwo\\nthree\\n'"},
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if resp.AgentID == "" {
		t.Fatal("StartAgent: empty agent id")
	}

	var (
		mu       sync.Mutex
		captured []StreamEvent
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.StreamEvents(ctx, resp.AgentID, 0, func(ev StreamEvent) error {
		mu.Lock()
		captured = append(captured, ev)
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	if len(captured) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(captured), captured)
	}
	wantLines := []string{"one", "two", "three"}
	for i, ev := range captured {
		if ev.Offset != int64(i+1) {
			t.Errorf("event %d: offset=%d want=%d", i, ev.Offset, i+1)
		}
		if ev.Line != wantLines[i] {
			t.Errorf("event %d: line=%q want=%q", i, ev.Line, wantLines[i])
		}
	}
}

// TestStreamFromOffsetResume covers AC bullet "SIGKILL mid-flight: simulate
// new worker resuming → events continuous, no duplicates". We start an
// agent, consume one line, then re-subscribe with from_offset=1 and assert
// the second subscriber sees only the remaining lines.
func TestStreamFromOffsetResume(t *testing.T) {
	_, srv := newTestServer(t)
	c := NewHTTP(srv.URL, srv.Client())

	resp, err := c.StartAgent(context.Background(), supervisor.StartAgentRequest{
		Runtime: "/bin/sh",
		Args:    []string{"-c", "printf 'a\\nb\\nc\\nd\\n'"},
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	// First subscriber: stop after 1 event by returning an error.
	stopErr := errors.New("done after first event")
	var first []StreamEvent
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	if err := c.StreamEvents(ctx1, resp.AgentID, 0, func(ev StreamEvent) error {
		first = append(first, ev)
		return stopErr
	}); !errors.Is(err, stopErr) {
		t.Fatalf("first stream err = %v, want %v", err, stopErr)
	}
	if len(first) != 1 || first[0].Offset != 1 {
		t.Fatalf("first stream got %+v, want offset 1", first)
	}

	// Resume from offset 1; expect 2,3,4 with no duplicate of offset 1.
	var second []StreamEvent
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := c.StreamEvents(ctx2, resp.AgentID, 1, func(ev StreamEvent) error {
		second = append(second, ev)
		return nil
	}); err != nil {
		t.Fatalf("resume stream: %v", err)
	}
	if len(second) != 3 {
		t.Fatalf("resume got %d events, want 3: %+v", len(second), second)
	}
	for i, ev := range second {
		want := int64(i + 2)
		if ev.Offset != want {
			t.Errorf("resume event %d: offset=%d want=%d", i, ev.Offset, want)
		}
	}
}

// TestStatusNotFound covers AC bullet "supervisor returns 404: worker
// reports task failed (does not pretend in-flight)". The client maps 404 to
// the sentinel ErrAgentNotFound so the worker resume path can flag the
// task definitively rather than retrying a transport error.
func TestStatusNotFound(t *testing.T) {
	_, srv := newTestServer(t)
	c := NewHTTP(srv.URL, srv.Client())

	if _, err := c.Status(context.Background(), "no-such-agent-id"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Status: err=%v want ErrAgentNotFound", err)
	}
	if err := c.Cancel(context.Background(), "no-such-agent-id"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Cancel: err=%v want ErrAgentNotFound", err)
	}
	if err := c.StreamEvents(context.Background(), "no-such-agent-id", 0, func(StreamEvent) error { return nil }); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("StreamEvents: err=%v want ErrAgentNotFound", err)
	}
}

// TestCancel verifies the client's Cancel wrapper makes the supervisor's
// SIGTERM/SIGKILL cycle observable: a long-sleeping subprocess transitions
// to status=exited shortly after Cancel returns 204.
func TestCancel(t *testing.T) {
	s, srv := newTestServer(t)
	c := NewHTTP(srv.URL, srv.Client())

	resp, err := c.StartAgent(context.Background(), supervisor.StartAgentRequest{
		Runtime: "/bin/sh",
		Args:    []string{"-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if err := c.Cancel(context.Background(), resp.AgentID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, ok := s.Status(resp.AgentID)
		if ok && st.Status == "exited" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("agent did not reach exited within 3s after cancel")
}
