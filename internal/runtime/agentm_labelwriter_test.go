package runtime

// Tests for the v0.6 coordinator-managed label writer hook (REQ-146 /
// #332). These cover the AgentM-only contract: on success+publish-OK we
// fire the LabelWriter with the agent-suggested label; on failure or
// non-AgentM runtimes we MUST NOT fire it. The fake AgentM binary from
// agentmtest drives the underlying agent.Session.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm/agentmtest"
	"github.com/Lincyaw/workbuddy/internal/config"
)

// fakeLabelWriter records every ApplyNextLabel call so the assertions can
// check both the routing and the strict ordering against the gitops fake.
type fakeLabelWriter struct {
	mu    sync.Mutex
	calls []labelCall
	err   error
}

type labelCall struct {
	repo     string
	issueNum int
	label    string
}

func (f *fakeLabelWriter) ApplyNextLabel(_ context.Context, repo string, issueNum int, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, labelCall{repo: repo, issueNum: issueNum, label: label})
	return f.err
}

func (f *fakeLabelWriter) Calls() []labelCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]labelCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// AC-1-1: AgentM happy path with a non-empty next_label and a configured
// GitOps + LabelWriter pair MUST trigger the label writer with the exact
// label the agent emitted, and stamp Result.Meta["agentm_label_applied"].
func TestAgentMBridge_AppliesNextLabelOnSuccess(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:      agentmtest.ModeSuccess,
		NextLabel: "status:reviewing",
	})
	gops := &fakeGitOps{prURL: "https://example.com/pull/1"}
	lw := &fakeLabelWriter{}
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})
	rt.GitOps = gops
	rt.LabelWriter = lw

	work := t.TempDir()
	task := &TaskContext{
		Repo:    "Lincyaw/workbuddy",
		WorkDir: work,
		Issue:   IssueContext{Number: 332, Title: "wire next_label"},
		Session: SessionContext{ID: "test-session"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := rt.Launch(ctx, &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeAgentM}, task)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	calls := lw.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 label-writer call, got %d", len(calls))
	}
	got := calls[0]
	if got.repo != "Lincyaw/workbuddy" || got.issueNum != 332 || got.label != "status:reviewing" {
		t.Fatalf("bad label call: %+v", got)
	}
	if res.Meta["agentm_label_applied"] != "status:reviewing" {
		t.Fatalf("agentm_label_applied meta = %q", res.Meta["agentm_label_applied"])
	}
	if res.Meta["agentm_next_label"] != "status:reviewing" {
		t.Fatalf("agentm_next_label meta = %q", res.Meta["agentm_next_label"])
	}
}

// AC-1-2: AgentM run with success=false MUST NOT fire the label writer.
// failure_reason still flows into Meta for the reporter to surface.
func TestAgentMBridge_NoLabelOnFailure(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:          agentmtest.ModeFailure,
		NextLabel:     "status:failed",
		FailureReason: "ac not met",
	})
	gops := &fakeGitOps{}
	lw := &fakeLabelWriter{}
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})
	rt.GitOps = gops
	rt.LabelWriter = lw

	work := t.TempDir()
	task := &TaskContext{
		Repo:    "Lincyaw/workbuddy",
		WorkDir: work,
		Issue:   IssueContext{Number: 332},
		Session: SessionContext{ID: "test-session"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := rt.Launch(ctx, &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeAgentM}, task); err == nil {
		t.Fatal("expected non-nil err on clean failure")
	}
	if calls := lw.Calls(); len(calls) != 0 {
		t.Fatalf("expected 0 label-writer calls on failure, got %v", calls)
	}
	if got := len(gops.calls); got != 0 {
		t.Fatalf("publish must also be skipped on failure, got %d calls", got)
	}
}

// Sequencing contract (matches the spec on #332): if gitops publish
// fails, the label writer MUST NOT fire — we cannot advance the state
// machine when the PR was not opened.
func TestAgentMBridge_NoLabelWhenPublishFails(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:      agentmtest.ModeSuccess,
		NextLabel: "status:reviewing",
	})
	gops := &fakeGitOps{err: errors.New("commit-push: permission denied")}
	lw := &fakeLabelWriter{}
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})
	rt.GitOps = gops
	rt.LabelWriter = lw

	work := t.TempDir()
	task := &TaskContext{
		Repo:    "Lincyaw/workbuddy",
		WorkDir: work,
		Issue:   IssueContext{Number: 332},
		Session: SessionContext{ID: "test-session"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, _ := rt.Launch(ctx, &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeAgentM}, task)
	if calls := lw.Calls(); len(calls) != 0 {
		t.Fatalf("expected 0 label-writer calls when publish fails, got %v", calls)
	}
	if !IsInfraFailure(res) {
		t.Fatalf("publish failure should mark infra failure, meta=%v", res.Meta)
	}
}

// Empty next_label must be a no-op: the agent didn't suggest a
// transition, so we don't invent one. We exercise applyAgentMNextLabel
// directly because the fake AgentM binary auto-fills the default label
// for ModeSuccess.
func TestAgentMBridge_EmptyNextLabelIsNoOp(t *testing.T) {
	lw := &fakeLabelWriter{}
	sess := &AgentBridgeSession{
		LabelWriter: lw,
		Task: &TaskContext{
			Repo:  "Lincyaw/workbuddy",
			Issue: IssueContext{Number: 332},
		},
	}
	got, err := sess.applyAgentMNextLabel(context.Background(), "   ")
	if err != nil {
		t.Fatalf("applyAgentMNextLabel: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty applied label for whitespace input, got %q", got)
	}
	if calls := lw.Calls(); len(calls) != 0 {
		t.Fatalf("expected 0 label-writer calls for empty next_label, got %v", calls)
	}
}

// AC-1-3 (runtime gate): claude-code / codex runtimes MUST NEVER touch
// the label writer regardless of what gets put in their Result.Meta. We
// model this at the structural level: the AgentBridge struct is the only
// path that consults LabelWriter, and only when the underlying Session
// is an AgentM session (the type-assertion gate in Run()). To pin the
// contract, build an AgentBridge against a claude/codex session-like
// fake and confirm no call goes through even when LabelWriter is wired.
//
// We can't easily construct a real claude/codex session in a unit test,
// but we CAN drive the bridge against an AgentM fake while declaring the
// outer runtime as something else — the runtime-name gate that matters
// for production lives in the registry adapter (only the AgentM
// AgentBridgeRuntime ever has LabelWriter set), so we additionally
// assert that path in TestRegistry_OnlyAgentMGetsLabelWriter.
func TestAgentMBridge_OtherRuntimesNeverWireLabelWriter(t *testing.T) {
	// Build a fake AgentM session but expose it through a non-AgentM
	// runtime name. The bridge type-asserts on the SESSION (agentm.Output
	// interface), not on the runtime name string, so this case actually
	// is "agentm session under a misconfigured runtime name". The real
	// runtime-name gate is at the registry layer; see launcher tests.
	//
	// What this test guarantees is: when the agent does NOT expose the
	// Output() / SessionLogPath() interface (i.e. claude-code or codex
	// sessions, which lack that method), the AgentBridge's
	// type-assertion `if extractor, ok := s.Session.(interface{...}); ok`
	// fails closed, the next_label branch is unreachable, and
	// LabelWriter is never called. Walk that branch with a session that
	// doesn't implement Output().
	rt := NewAgentBridgeRuntime(config.RuntimeClaudeCode, func() (agent.Backend, error) {
		return &nopBackend{}, nil
	})
	lw := &fakeLabelWriter{}
	rt.LabelWriter = lw

	work := t.TempDir()
	task := &TaskContext{
		Repo:    "Lincyaw/workbuddy",
		WorkDir: work,
		Issue:   IssueContext{Number: 332},
		Session: SessionContext{ID: "test-session"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rt.Launch(ctx, &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeClaudeCode}, task); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if calls := lw.Calls(); len(calls) != 0 {
		t.Fatalf("non-AgentM session must never invoke LabelWriter, got %v", calls)
	}
}

// nopBackend is a minimal agent.Backend whose session implements just
// enough of agent.Session to drive the bridge through Run/Wait. It does
// NOT implement Output()/SessionLogPath(), which is the point: the
// bridge's type-assertion gate keeps the AgentM-specific Meta + label
// path unreachable.
type nopBackend struct{}

func (b *nopBackend) NewSession(_ context.Context, _ agent.Spec) (agent.Session, error) {
	return &nopSession{events: make(chan agent.Event)}, nil
}
func (b *nopBackend) Shutdown(_ context.Context) error { return nil }

type nopSession struct {
	events chan agent.Event
	closed bool
	mu     sync.Mutex
}

func (s *nopSession) ID() string                         { return "nop-session" }
func (s *nopSession) Events() <-chan agent.Event         { return s.events }
func (s *nopSession) Interrupt(_ context.Context) error  { return nil }
func (s *nopSession) Wait(_ context.Context) (agent.Result, error) {
	s.mu.Lock()
	if !s.closed {
		close(s.events)
		s.closed = true
	}
	s.mu.Unlock()
	return agent.Result{ExitCode: 0}, nil
}
func (s *nopSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		close(s.events)
		s.closed = true
	}
	return nil
}
