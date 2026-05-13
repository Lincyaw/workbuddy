// Tests for long-lifecycle OTel correlation (REQ-138 / #320).
//
// AC coverage:
//   - AC-1-1: a single issue ingested then dispatched twice — every
//     span emitted across the coordinator-side pipeline shares the
//     issue's persisted root_trace_id.
//   - AC-1-2: a PR whose ParentIssueNum points at the issue inherits
//     the trace_id; PR-side spans correlate with the issue.
//   - AC-1-3: every span on the affected paths carries the mandatory
//     business attributes (repo, issue.id, issue.number, plus
//     pr.number / agent.role / agent.runtime when applicable).

package statemachine

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func installTraceRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return rec
}

func attrMap(span sdktrace.ReadOnlySpan) map[string]string {
	out := make(map[string]string)
	for _, a := range span.Attributes() {
		out[string(a.Key)] = a.Value.Emit()
	}
	return out
}

// AC-1-1: spans across operations on the same issue share trace_id.
func TestIssueSpansShareRootTraceID(t *testing.T) {
	rec := installTraceRecorder(t)

	sm, _, dispatch := newTestSM(t)
	const repo = "owner/repo"
	const issueNum = 42

	// Seed issue_cache so UpsertIssueCache mints the trace_id.
	if err := sm.store.UpsertIssueCache(store.IssueCache{
		Repo: repo, IssueNum: issueNum, Labels: `["workbuddy"]`, State: "open",
	}); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	expectedTID, err := sm.store.GetIssueRootTraceID(repo, issueNum)
	if err != nil || expectedTID == "" {
		t.Fatalf("missing seeded trace_id: %v / %q", err, expectedTID)
	}

	// Dispatch #1: label_added moves issue into developing → triggers
	// dispatch to dev-agent.
	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:developing",
	}); err != nil {
		t.Fatalf("HandleEvent #1: %v", err)
	}
	// Drain dispatch so the channel doesn't back up.
	select {
	case req := <-dispatch:
		if req.RootTraceID != expectedTID {
			t.Errorf("dispatch #1 RootTraceID = %q, want %q", req.RootTraceID, expectedTID)
		}
	default:
	}

	// Reset dedup so the second event isn't suppressed (same key would
	// otherwise hit the processedEvents cache).
	sm.ResetDedup()

	// Dispatch #2: label_added moves issue into reviewing.
	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   []string{"workbuddy", "status:reviewing"},
		Detail:   "status:reviewing",
	}); err != nil {
		t.Fatalf("HandleEvent #2: %v", err)
	}
	select {
	case req := <-dispatch:
		if req.RootTraceID != expectedTID {
			t.Errorf("dispatch #2 RootTraceID = %q, want %q", req.RootTraceID, expectedTID)
		}
	default:
	}

	// Every captured span tied to this issue must share trace_id.
	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans captured")
	}
	saw := 0
	for _, sp := range spans {
		got := sp.SpanContext().TraceID().String()
		if got != expectedTID {
			t.Errorf("span %q trace_id = %q, want %q", sp.Name(), got, expectedTID)
		}
		saw++
	}
	if saw < 2 {
		// Expect at least two handleEvent spans plus dispatch spans.
		t.Fatalf("expected at least 2 spans correlated to issue %d, got %d", issueNum, saw)
	}
}

// AC-1-2: a PR linked to the issue (via ParentIssueNum) inherits the
// trace_id; spans emitted while handling PR events use the same trace.
func TestPRSpansInheritIssueTraceID(t *testing.T) {
	rec := installTraceRecorder(t)

	sm, _, _ := newTestSM(t)
	const repo = "owner/repo"
	const issueNum = 7
	const prNum = 107

	// Seed parent issue + linked PR.
	if err := sm.store.UpsertIssueCache(store.IssueCache{
		Repo: repo, IssueNum: issueNum, Labels: `["workbuddy"]`, State: "open",
	}); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	issueTID, _ := sm.store.GetIssueRootTraceID(repo, issueNum)
	if issueTID == "" {
		t.Fatal("issue trace_id was not minted")
	}

	if err := sm.store.UpsertIssueCache(store.IssueCache{
		Repo: repo, IssueNum: prNum, State: "pr:open", ParentIssueNum: issueNum,
	}); err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	prTID, _ := sm.store.GetPRRootTraceID(repo, prNum)
	if prTID != issueTID {
		t.Fatalf("PR trace_id = %q did not inherit issue trace_id %q", prTID, issueTID)
	}

	// Fire a PR-side event. Even though our test workflow doesn't have
	// a PR-specific trigger, HandleEvent still emits a span carrying
	// the inherited trace_id.
	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventPRStateChanged,
		Repo:     repo,
		IssueNum: prNum, // PRs are keyed by PR number in issue_cache
		Detail:   "pr:open -> pr:closed",
	}); err != nil {
		t.Fatalf("HandleEvent PR: %v", err)
	}

	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans captured")
	}
	// At least one captured span should have the inherited trace_id —
	// the handleEvent span for the PR event.
	hits := 0
	for _, sp := range spans {
		if sp.SpanContext().TraceID().String() == issueTID {
			hits++
		}
	}
	if hits == 0 {
		t.Fatalf("no PR-side span carried the inherited trace_id %q (captured %d spans)", issueTID, len(spans))
	}
}

// AC-1-3: mandatory business attributes are present on the
// statemachine.handleEvent and dispatchAgent spans.
func TestSpansCarryMandatoryAttributes(t *testing.T) {
	rec := installTraceRecorder(t)

	sm, _, dispatch := newTestSM(t)
	const repo = "owner/repo"
	const issueNum = 11

	if err := sm.store.UpsertIssueCache(store.IssueCache{
		Repo: repo, IssueNum: issueNum, Labels: `["workbuddy"]`, State: "open",
	}); err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:developing",
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	// Drain dispatch.
	select {
	case <-dispatch:
	default:
	}

	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans captured")
	}

	// Every captured span on the affected issue paths must carry the
	// standard keys (repo, issue.id, issue.number). The trace-id minting
	// helper (`store.mint_root_trace_id`) is intentionally excluded — it
	// runs at row-insert time, before the issue context is available
	// and is not a per-operation span.
	required := []string{"repo", "issue.id", "issue.number"}
	checked := 0
	for _, sp := range spans {
		if sp.Name() == "store.mint_root_trace_id" {
			continue
		}
		attrs := attrMap(sp)
		for _, k := range required {
			if attrs[k] == "" {
				t.Errorf("span %q missing required attr %q (have %v)", sp.Name(), k, attrs)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no issue-context spans captured to verify attrs against")
	}
}
