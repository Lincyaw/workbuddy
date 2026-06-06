package runtime

// Tests for the v0.6 coordinator-managed label writer hook (REQ-146 /
// #332). AgentM runs in autonomous mode — the agent does its own git
// push / PR — so the label transition is the only Go-side state-machine
// write. These cover the AgentM-only contract: on a valid non-empty
// next_label we fire the LabelWriter with the agent-suggested label,
// regardless of any Go-side publish; on an empty next_label or
// non-AgentM runtimes we MUST NOT fire it. The fake AgentM binary from
// agentmtest drives the underlying agent.Session.

import (
	"context"
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
// LabelWriter MUST trigger the label writer with the exact label the
// agent emitted, and stamp Result.Meta["agentm_label_applied"].
func TestAgentMBridge_AppliesNextLabelOnSuccess(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:      agentmtest.ModeResultSuccess,
		NextLabel: "status:reviewing",
	})
	lw := &fakeLabelWriter{}
	rt := NewAgentMRuntime(func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})
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

// AC-1-2: AgentM run with success=false still applies next_label — a
// review-agent bouncing an issue back to developing is a legitimate
// routing decision. failure_reason still flows into Meta for the reporter
// to surface.
func TestAgentMBridge_LabelAppliedOnFailure(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:          agentmtest.ModeFailure,
		NextLabel:     "status:developing",
		FailureReason: "ac not met",
	})
	lw := &fakeLabelWriter{}
	rt := NewAgentMRuntime(func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})
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
	calls := lw.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 label-writer call on failure (routing decision), got %v", calls)
	}
	if calls[0].label != "status:developing" {
		t.Fatalf("expected label %q, got %q", "status:developing", calls[0].label)
	}
}

// Empty next_label must be a no-op: the agent didn't suggest a
// transition, so we don't invent one. We exercise applyAgentMNextLabel
// directly because the fake AgentM binary auto-fills the default label
// for ModeSuccess.
func TestAgentMBridge_EmptyNextLabelIsNoOp(t *testing.T) {
	lw := &fakeLabelWriter{}
	sess := &AgentMSession{
		LabelWriter: lw,
		AgentBridgeSession: &AgentBridgeSession{
			Task: &TaskContext{
				Repo:  "Lincyaw/workbuddy",
				Issue: IssueContext{Number: 332},
			},
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

// AC-1-3 (capability gate): claude-code / codex runtimes MUST NEVER touch
// the label writer. After the §1/§3 split this is structural, not a runtime
// -name check: the self-managed host-exec bridge (codex/claude) is a plain
// AgentBridgeRuntime with NO LabelWriter field and an AgentBridgeSession that
// carries no Output() extraction at all — only the agentm AgentMRuntime /
// AgentMSession carry the label path. This test pins the capability contract
// that drives the wiring: the bridge core reports ManagesOwnLabels=true, so
// Registry.SetLabelWriter skips it; and a codex-style session (one that does
// not implement agentm.Output) produces no agentm label meta. The registry
// -level wiring assertion lives in TestRegistry_OnlyAgentMGetsLabelWriter.
func TestAgentMBridge_OtherRuntimesNeverWireLabelWriter(t *testing.T) {
	rt := NewAgentBridgeRuntime(config.RuntimeCodex, func() (agent.Backend, error) {
		return &nopBackend{}, nil
	})

	// Capability gate: self-managed runtimes own their own labels, so the
	// capability-driven SetLabelWriter never wires them.
	if !rt.Capabilities().ManagesOwnLabels {
		t.Fatalf("self-managed bridge runtime must report ManagesOwnLabels=true")
	}

	work := t.TempDir()
	task := &TaskContext{
		Repo:    "Lincyaw/workbuddy",
		WorkDir: work,
		Issue:   IssueContext{Number: 332},
		Session: SessionContext{ID: "test-session"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := rt.Launch(ctx, &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeCodex}, task)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// A non-agentm session never produces the agentm label/next_label meta:
	// the AgentBridgeSession carries no Output() extraction path.
	for _, k := range []string{"agentm_next_label", "agentm_label_applied", "agentm_label_error"} {
		if _, ok := res.Meta[k]; ok {
			t.Fatalf("non-AgentM session must not emit %q meta, got %v", k, res.Meta)
		}
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
