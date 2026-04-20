package claude

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/agent"
)

func TestClaudeNewBackend(t *testing.T) {
	b := NewBackend()
	if b == nil {
		t.Fatal("NewBackend() returned nil")
	}

	// Verify it satisfies agent.Backend.
	var _ agent.Backend = b
}

func TestClaudeBackendShutdownNoop(t *testing.T) {
	b := NewBackend()
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v, want nil", err)
	}
}

func TestClaudeSessionEvents(t *testing.T) {
	// Skip if claude binary is not available.
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not available, skipping")
	}

	b := NewBackend()
	spec := agent.Spec{
		Prompt: "echo hello",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := b.NewSession(ctx, spec)
	if err != nil {
		t.Fatalf("NewSession() error: %v", err)
	}
	defer func() { _ = sess.Close() }()

	ch := sess.Events()
	if ch == nil {
		t.Fatal("Events() returned nil channel")
	}

	// Cancel immediately since we're just checking the channel exists.
	cancel()
}

func TestMapClaudeEventValidJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantKind string
	}{
		{
			name:     "system init",
			input:    `{"type":"system.init","data":{}}`,
			wantKind: "turn.started",
		},
		{
			name:     "assistant message delta",
			input:    `{"type":"assistant.content_block_delta","delta":{"text":"hi"}}`,
			wantKind: "agent.message",
		},
		{
			name:     "message stop",
			input:    `{"type":"assistant.message_stop"}`,
			wantKind: "turn.completed",
		},
		{
			name:     "tool call start",
			input:    `{"type":"assistant.content_block_start"}`,
			wantKind: "tool.call",
		},
		{
			name:     "tool result",
			input:    `{"type":"user.tool_result","result":"ok"}`,
			wantKind: "tool.result",
		},
		{
			name:     "error event",
			input:    `{"type":"system.error","error":"boom"}`,
			wantKind: "error",
		},
		{
			name:     "message start internal",
			input:    `{"type":"assistant.message_start"}`,
			wantKind: "internal",
		},
		{
			name:     "unknown type",
			input:    `{"type":"something.new"}`,
			wantKind: "log",
		},
		{
			name:     "no type field",
			input:    `{"data":"value"}`,
			wantKind: "log",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := mapClaudeEvent([]byte(tt.input))
			if evt.Kind != tt.wantKind {
				t.Fatalf("mapClaudeEvent(%s).Kind = %q, want %q", tt.input, evt.Kind, tt.wantKind)
			}
			// Body should be valid JSON.
			if !json.Valid(evt.Body) {
				t.Fatalf("mapClaudeEvent(%s).Body is not valid JSON: %s", tt.input, evt.Body)
			}
		})
	}
}

func TestMapClaudeEventInvalidJSON(t *testing.T) {
	evt := mapClaudeEvent([]byte("not json at all"))
	if evt.Kind != "log" {
		t.Fatalf("mapClaudeEvent(invalid).Kind = %q, want %q", evt.Kind, "log")
	}
}

func TestExtractFinalMessage(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "with text field",
			input: `{"text":"  final answer  "}`,
			want:  "final answer",
		},
		{
			name:  "no text field",
			input: `{"data":"value"}`,
			want:  "",
		},
		{
			name:  "invalid json",
			input: `not json`,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractFinalMessage(json.RawMessage(tt.input))
			if got != tt.want {
				t.Fatalf("extractFinalMessage(%s) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapKind(t *testing.T) {
	tests := []struct {
		claudeType string
		want       string
	}{
		{"system.init", "turn.started"},
		{"assistant.content_block_delta", "agent.message"},
		{"assistant.message_stop", "turn.completed"},
		{"assistant.content_block_start", "tool.call"},
		{"user.tool_result", "tool.result"},
		{"system.error", "error"},
		{"assistant.message_start", "internal"},
		{"assistant.content_block_stop", "internal"},
		{"assistant.message_delta", "internal"},
		{"", "log"},
		{"unknown.type", "log"},
	}

	for _, tt := range tests {
		t.Run(tt.claudeType, func(t *testing.T) {
			got := mapKind(tt.claudeType)
			if got != tt.want {
				t.Fatalf("mapKind(%q) = %q, want %q", tt.claudeType, got, tt.want)
			}
		})
	}
}
