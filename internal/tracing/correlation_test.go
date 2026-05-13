// Tests for the long-lifecycle correlation helpers (REQ-138 / #320).
//
// AC coverage:
//   - AC-1-1 (helper basics): a valid traceIDHex produces a derived ctx
//     whose next span carries the persisted trace_id.
//   - AC-1-3 (attrs helper): SetIssueAttrs stamps the documented keys
//     and skips empties.
//   - malformed/empty trace_id is a no-op (ctx unchanged, no panic).

package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func newTracetestProvider(t *testing.T) *tracetest.SpanRecorder {
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

func TestContextFromTraceID_ValidIDRoots(t *testing.T) {
	rec := newTracetestProvider(t)
	const tidHex = "0123456789abcdef0123456789abcdef"

	ctx := ContextFromTraceID(context.Background(), tidHex)
	_, span := Start(ctx, "test.child")
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	got := spans[0].SpanContext().TraceID().String()
	if got != tidHex {
		t.Fatalf("child span trace_id = %q, want %q", got, tidHex)
	}
	// The child must be a child of our synthetic remote parent, not
	// the root of a fresh trace.
	parent := spans[0].Parent()
	if !parent.IsValid() {
		t.Fatal("child span has no parent SpanContext")
	}
	if !parent.IsRemote() {
		t.Fatal("parent SpanContext should be marked remote (synthetic root)")
	}
	if parent.TraceID().String() != tidHex {
		t.Fatalf("parent trace_id = %q, want %q", parent.TraceID().String(), tidHex)
	}
}

func TestContextFromTraceID_EmptyIsNoop(t *testing.T) {
	_ = newTracetestProvider(t)
	ctx := context.Background()
	got := ContextFromTraceID(ctx, "")
	// Empty input must not install a parent SpanContext.
	sc := trace.SpanContextFromContext(got)
	if sc.IsValid() {
		t.Fatalf("empty trace_id should leave ctx untouched, got valid SpanContext %s", sc.TraceID())
	}
}

func TestContextFromTraceID_MalformedIsNoop(t *testing.T) {
	_ = newTracetestProvider(t)
	cases := []string{
		"not-hex",
		"0123",                                  // too short
		"0123456789abcdef0123456789abcdefAA",    // too long
		"00000000000000000000000000000000",      // all-zero (invalid TraceID)
		"GGGG456789abcdef0123456789abcdef",      // non-hex
	}
	for _, in := range cases {
		ctx := ContextFromTraceID(context.Background(), in)
		sc := trace.SpanContextFromContext(ctx)
		if sc.IsValid() {
			t.Fatalf("malformed input %q should not install SpanContext", in)
		}
	}
}

func TestSetIssueAttrs_FullPath(t *testing.T) {
	rec := newTracetestProvider(t)
	_, span := Start(context.Background(), "test.attrs")
	SetIssueAttrs(span, "owner/repo", 42, 100, "dev", "claude-code")
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	got := map[string]string{}
	for _, a := range spans[0].Attributes() {
		got[string(a.Key)] = a.Value.Emit()
	}
	want := map[string]string{
		"repo":          "owner/repo",
		"issue.id":      "owner/repo#42",
		"issue.number":  "42",
		"pr.number":     "100",
		"agent.role":    "dev",
		"agent.runtime": "claude-code",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("attr %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestSetIssueAttrs_SkipsEmpties(t *testing.T) {
	rec := newTracetestProvider(t)
	_, span := Start(context.Background(), "test.attrs.partial")
	// Only issue, no PR / role / runtime.
	SetIssueAttrs(span, "owner/repo", 7, 0, "", "")
	span.End()

	spans := rec.Ended()
	got := map[string]bool{}
	for _, a := range spans[0].Attributes() {
		got[string(a.Key)] = true
	}
	for _, mustHave := range []string{"repo", "issue.id", "issue.number"} {
		if !got[mustHave] {
			t.Errorf("missing required attr %s", mustHave)
		}
	}
	for _, mustSkip := range []string{"pr.number", "agent.role", "agent.runtime"} {
		if got[mustSkip] {
			t.Errorf("attr %s should be skipped when empty", mustSkip)
		}
	}
}
