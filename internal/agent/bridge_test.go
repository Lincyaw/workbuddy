package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

// --- Mock types ---

type mockSession struct {
	id     string
	events chan Event
	result Result
	err    error
	// done signals that Wait should return. The bridge goroutine reads from
	// Events(), so Wait must NOT drain the channel -- it just blocks on done.
	done chan struct{}
}

func newMockSession(id string, result Result) *mockSession {
	return &mockSession{
		id:     id,
		events: make(chan Event, 16),
		result: result,
		done:   make(chan struct{}),
	}
}

func (s *mockSession) ID() string                      { return s.id }
func (s *mockSession) Events() <-chan Event             { return s.events }
func (s *mockSession) Interrupt(context.Context) error  { return nil }
func (s *mockSession) Close() error                     { return nil }

func (s *mockSession) Wait(_ context.Context) (Result, error) {
	<-s.done
	return s.result, s.err
}

type mockHandle struct {
	written [][]byte
}

func (h *mockHandle) WriteStdout(data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	h.written = append(h.written, cp)
	return nil
}

// --- Tests ---

func TestTranslateKind(t *testing.T) {
	tests := []struct {
		kind string
		want launcherevents.EventKind
	}{
		{"turn.started", launcherevents.KindTurnStarted},
		{"turn.completed", launcherevents.KindTurnCompleted},
		{"agent.message", launcherevents.KindAgentMessage},
		{"tool.call", launcherevents.KindToolCall},
		{"tool.result", launcherevents.KindToolResult},
		{"error", launcherevents.KindError},
		{"reasoning", launcherevents.KindReasoning},
		{"command.exec", launcherevents.KindCommandExec},
		{"command.output", launcherevents.KindCommandOutput},
		{"file.change", launcherevents.KindFileChange},
		{"token.usage", launcherevents.KindTokenUsage},
		{"task.complete", launcherevents.KindTaskComplete},
		{"log", launcherevents.KindLog},
		{"internal", ""},                   // skip
		{"unknown.kind", launcherevents.KindLog}, // default to log
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			got := translateKind(tt.kind)
			if got != tt.want {
				t.Fatalf("translateKind(%q) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

func TestBridgeSessionRun(t *testing.T) {
	sess := newMockSession("test-session-1", Result{
		ExitCode: 0,
		FinalMsg: "all done",
		Duration: 5 * time.Second,
	})

	handle := &mockHandle{}
	bs := &BridgeSession{Sess: sess, Handle: handle}

	// Send some events then close channel and signal done.
	sess.events <- Event{Kind: "turn.started", Body: json.RawMessage(`{"msg":"start"}`)}
	sess.events <- Event{Kind: "agent.message", Body: json.RawMessage(`{"text":"hello"}`)}
	sess.events <- Event{Kind: "turn.completed", Body: json.RawMessage(`{"msg":"end"}`)}
	close(sess.events)

	// Signal done after a brief delay so the goroutine can drain events.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(sess.done)
	}()

	outCh := make(chan launcherevents.Event, 16)
	result, err := bs.Run(context.Background(), outCh)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.LastMessage != "all done" {
		t.Fatalf("LastMessage = %q, want %q", result.LastMessage, "all done")
	}
	if result.Duration != 5*time.Second {
		t.Fatalf("Duration = %v, want %v", result.Duration, 5*time.Second)
	}

	// Verify handle received JSONL.
	if len(handle.written) != 3 {
		t.Fatalf("handle.written count = %d, want 3", len(handle.written))
	}
}

func TestBridgeSessionRunNilEvents(t *testing.T) {
	sess := newMockSession("test-session-2", Result{ExitCode: 0})
	bs := &BridgeSession{Sess: sess, Handle: nil}

	sess.events <- Event{Kind: "agent.message", Body: json.RawMessage(`{"text":"hi"}`)}
	close(sess.events)
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(sess.done)
	}()

	// Pass nil events channel -- should not panic.
	result, err := bs.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
}

func TestBridgeResultTranslation(t *testing.T) {
	sess := newMockSession("test-session-3", Result{
		ExitCode:     1,
		FinalMsg:     "error occurred",
		FilesChanged: []string{"a.go", "b.go", "c.go"},
		Duration:     10 * time.Second,
	})

	bs := &BridgeSession{Sess: sess}
	close(sess.events)
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(sess.done)
	}()

	result, err := bs.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if result.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", result.ExitCode)
	}
	if result.LastMessage != "error occurred" {
		t.Fatalf("LastMessage = %q, want %q", result.LastMessage, "error occurred")
	}
	if result.Meta == nil {
		t.Fatal("Meta is nil, want files_changed")
	}
	if result.Meta["files_changed"] != "a.go,b.go,c.go" {
		t.Fatalf("Meta[files_changed] = %q, want %q", result.Meta["files_changed"], "a.go,b.go,c.go")
	}
}

func TestBridgeResultNoFilesChanged(t *testing.T) {
	sess := newMockSession("test-session-4", Result{ExitCode: 0})
	bs := &BridgeSession{Sess: sess}
	close(sess.events)
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(sess.done)
	}()

	result, err := bs.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.Meta != nil {
		t.Fatalf("Meta = %v, want nil when no files changed", result.Meta)
	}
}

func TestBridgeEventTranslation(t *testing.T) {
	sess := newMockSession("test-session-5", Result{ExitCode: 0})
	bs := &BridgeSession{Sess: sess}

	testEvents := []Event{
		{Kind: "turn.started", Body: json.RawMessage(`{}`)},
		{Kind: "agent.message", Body: json.RawMessage(`{"text":"hi"}`)},
		{Kind: "internal", Body: json.RawMessage(`{}`)}, // should be skipped
		{Kind: "tool.call", Body: json.RawMessage(`{"name":"bash"}`)},
		{Kind: "turn.completed", Body: json.RawMessage(`{}`)},
	}

	for _, e := range testEvents {
		sess.events <- e
	}
	close(sess.events)

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(sess.done)
	}()

	outCh := make(chan launcherevents.Event, 16)
	_, err := bs.Run(context.Background(), outCh)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Collect output events.
	close(outCh)
	var got []launcherevents.Event
	for e := range outCh {
		got = append(got, e)
	}

	// "internal" should be skipped, so expect 4 events.
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4 (internal skipped)", len(got))
	}

	expectedKinds := []launcherevents.EventKind{
		launcherevents.KindTurnStarted,
		launcherevents.KindAgentMessage,
		launcherevents.KindToolCall,
		launcherevents.KindTurnCompleted,
	}
	for i, want := range expectedKinds {
		if got[i].Kind != want {
			t.Fatalf("event[%d].Kind = %q, want %q", i, got[i].Kind, want)
		}
	}

	// Verify sequential seq numbers.
	for i, e := range got {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event[%d].Seq = %d, want %d", i, e.Seq, i+1)
		}
	}

	// Verify session ID.
	for i, e := range got {
		if e.SessionID != "test-session-5" {
			t.Fatalf("event[%d].SessionID = %q, want %q", i, e.SessionID, "test-session-5")
		}
	}
}

func TestBridgeClose(t *testing.T) {
	sess := newMockSession("test-session-6", Result{})
	bs := &BridgeSession{Sess: sess}
	if err := bs.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
}
