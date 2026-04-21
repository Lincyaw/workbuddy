package launcher

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

func TestTranslateAgentEventKind(t *testing.T) {
	tests := []struct {
		kind string
		want launcherevents.EventKind
	}{
		{kind: "turn.started", want: launcherevents.KindTurnStarted},
		{kind: "agent.message", want: launcherevents.KindAgentMessage},
		{kind: "internal", want: ""},
		{kind: "unknown", want: launcherevents.KindLog},
	}
	for _, tt := range tests {
		if got := translateAgentEventKind(tt.kind); got != tt.want {
			t.Fatalf("translateAgentEventKind(%q) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestAgentBridgeSessionRunTranslatesEventsAndWritesRaw(t *testing.T) {
	sess := &fakeAgentSession{
		id: "backend-session",
		result: agent.Result{
			ExitCode:     0,
			FinalMsg:     "done",
			FilesChanged: []string{"a.go", "b.go"},
			Duration:     5 * time.Second,
			SessionRef:   agent.SessionRef{ID: "thread-1", Kind: "codex-thread"},
		},
		events: make(chan agent.Event, 4),
		done:   make(chan struct{}),
	}
	handle := &fakeBridgeHandle{}
	bridge := &agentBridgeSession{
		session:  sess,
		handle:   handle,
		agentCfg: &config.AgentConfig{Name: "dev-agent"},
		task:     &TaskContext{Session: SessionContext{ID: "wb-session-1"}},
	}

	sess.events <- agent.Event{Kind: "agent.message", TurnID: "turn-1", Body: json.RawMessage(`{"text":"hi"}`), Raw: json.RawMessage(`{"raw":"hi"}`)}
	close(sess.events)
	close(sess.done)

	out := make(chan launcherevents.Event, 4)
	result, err := bridge.Run(context.Background(), out)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	close(out)

	if result == nil || result.LastMessage != "done" {
		t.Fatalf("result = %#v, want final message", result)
	}
	if result.Meta["files_changed"] != "a.go,b.go" {
		t.Fatalf("files_changed = %q", result.Meta["files_changed"])
	}
	if len(handle.stdout) != 1 || string(handle.stdout[0]) != "{\"raw\":\"hi\"}\n" {
		t.Fatalf("raw writes = %q", handle.stdout)
	}
	var events []launcherevents.Event
	for evt := range out {
		events = append(events, evt)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2 (permission + translated event)", len(events))
	}
	if events[1].Kind != launcherevents.KindAgentMessage {
		t.Fatalf("translated kind = %q, want %q", events[1].Kind, launcherevents.KindAgentMessage)
	}
}

type fakeAgentSession struct {
	id     string
	result agent.Result
	events chan agent.Event
	done   chan struct{}
}

func (f *fakeAgentSession) Events() <-chan agent.Event      { return f.events }
func (f *fakeAgentSession) Interrupt(context.Context) error { return nil }
func (f *fakeAgentSession) Close() error                    { return nil }
func (f *fakeAgentSession) ID() string                      { return f.id }
func (f *fakeAgentSession) Wait(context.Context) (agent.Result, error) {
	<-f.done
	return f.result, nil
}

type fakeBridgeHandle struct {
	stdout [][]byte
}

func (f *fakeBridgeHandle) WriteStdout(data []byte) error {
	cp := append([]byte(nil), data...)
	f.stdout = append(f.stdout, cp)
	return nil
}

func (f *fakeBridgeHandle) StdoutPath() string { return "/tmp/raw.jsonl" }
