package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/agent"
)

// Compile-time interface checks.
var _ agent.Session = (*fakeSession)(nil)
var _ agent.Backend = (*fakeBackend)(nil)

// fakeSession is a minimal Session implementation used only for compile checks.
type fakeSession struct{}

func (s *fakeSession) Events() <-chan agent.Event { return nil }
func (s *fakeSession) Wait(context.Context) (agent.Result, error) {
	return agent.Result{}, nil
}
func (s *fakeSession) Interrupt(context.Context) error { return nil }
func (s *fakeSession) Close() error                    { return nil }
func (s *fakeSession) ID() string                      { return "fake" }

// fakeBackend is a minimal Backend implementation used only for compile checks.
type fakeBackend struct{}

func (b *fakeBackend) NewSession(context.Context, agent.Spec) (agent.Session, error) {
	return &fakeSession{}, nil
}
func (b *fakeBackend) Shutdown(context.Context) error { return nil }

func TestSpecZeroValue(t *testing.T) {
	var s agent.Spec
	if s.Backend != "" {
		t.Fatalf("zero Spec.Backend = %q, want empty", s.Backend)
	}
	if s.Workdir != "" {
		t.Fatalf("zero Spec.Workdir = %q, want empty", s.Workdir)
	}
	if s.Prompt != "" {
		t.Fatalf("zero Spec.Prompt = %q, want empty", s.Prompt)
	}
	if s.Model != "" {
		t.Fatalf("zero Spec.Model = %q, want empty", s.Model)
	}
	if s.Sandbox != "" {
		t.Fatalf("zero Spec.Sandbox = %q, want empty", s.Sandbox)
	}
	if s.Approval != "" {
		t.Fatalf("zero Spec.Approval = %q, want empty", s.Approval)
	}
	if s.Env != nil {
		t.Fatalf("zero Spec.Env = %v, want nil", s.Env)
	}
	if s.Tags != nil {
		t.Fatalf("zero Spec.Tags = %v, want nil", s.Tags)
	}
}

func TestEventFields(t *testing.T) {
	body := json.RawMessage(`{"message":"hello"}`)
	raw := json.RawMessage(`{"method":"item/agentMessage/delta"}`)
	e := agent.Event{Kind: "agent.message", TurnID: "turn-1", Body: body, Raw: raw}

	if e.Kind != "agent.message" {
		t.Fatalf("Event.Kind = %q, want %q", e.Kind, "agent.message")
	}
	if e.TurnID != "turn-1" {
		t.Fatalf("Event.TurnID = %q, want %q", e.TurnID, "turn-1")
	}
	if string(e.Body) != `{"message":"hello"}` {
		t.Fatalf("Event.Body = %s, want %s", e.Body, body)
	}
	if string(e.Raw) != string(raw) {
		t.Fatalf("Event.Raw = %s, want %s", e.Raw, raw)
	}
}

func TestResultFields(t *testing.T) {
	r := agent.Result{
		ExitCode:     1,
		FinalMsg:     "done",
		FilesChanged: []string{"a.go", "b.go"},
		SessionRef:   agent.SessionRef{ID: "thread-1", Kind: "codex-thread"},
	}
	if r.ExitCode != 1 {
		t.Fatalf("Result.ExitCode = %d, want 1", r.ExitCode)
	}
	if r.FinalMsg != "done" {
		t.Fatalf("Result.FinalMsg = %q, want %q", r.FinalMsg, "done")
	}
	if len(r.FilesChanged) != 2 {
		t.Fatalf("Result.FilesChanged len = %d, want 2", len(r.FilesChanged))
	}
	if r.SessionRef.ID != "thread-1" {
		t.Fatalf("Result.SessionRef.ID = %q, want %q", r.SessionRef.ID, "thread-1")
	}
}

func TestSpecJSON(t *testing.T) {
	s := agent.Spec{
		Backend:  "claude",
		Workdir:  "/tmp/work",
		Prompt:   "fix the bug",
		Model:    "opus",
		Approval: "never",
		Env:      map[string]string{"FOO": "bar"},
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal Spec: %v", err)
	}

	var got agent.Spec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal Spec: %v", err)
	}
	if got.Backend != s.Backend {
		t.Fatalf("roundtrip Backend = %q, want %q", got.Backend, s.Backend)
	}
	if got.Prompt != s.Prompt {
		t.Fatalf("roundtrip Prompt = %q, want %q", got.Prompt, s.Prompt)
	}
	if got.Approval != s.Approval {
		t.Fatalf("roundtrip Approval = %q, want %q", got.Approval, s.Approval)
	}
	if got.Env["FOO"] != "bar" {
		t.Fatalf("roundtrip Env[FOO] = %q, want %q", got.Env["FOO"], "bar")
	}
}
