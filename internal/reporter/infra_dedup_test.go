package reporter

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

// dedupWriter implements GHCommentWriter and GHCommentEditor for tests.
// It records every WriteComment / EditComment call so assertions can
// observe the dedup logic's effect on the wire.
type dedupWriter struct {
	writes []dedupCall
	edits  []dedupCall
	nextID atomic.Int64
}

type dedupCall struct {
	repo      string
	issueNum  int
	commentID int64
	body      string
}

func (w *dedupWriter) WriteComment(repo string, issueNum int, body string) error {
	w.writes = append(w.writes, dedupCall{repo: repo, issueNum: issueNum, body: body})
	return nil
}

func (w *dedupWriter) WriteCommentReturningID(repo string, issueNum int, body string) (int64, error) {
	id := w.nextID.Add(1) + 1000 // start at 1001 to avoid 0 ambiguity
	w.writes = append(w.writes, dedupCall{repo: repo, issueNum: issueNum, body: body, commentID: id})
	return id, nil
}

func (w *dedupWriter) EditComment(repo string, commentID int64, body string) error {
	w.edits = append(w.edits, dedupCall{repo: repo, commentID: commentID, body: body})
	return nil
}

func newInfraResult(reason string) *runtimepkg.Result {
	r := &runtimepkg.Result{
		ExitCode: 1,
		Stderr:   reason,
		Meta:     map[string]string{},
	}
	runtimepkg.MarkInfraFailure(r, reason)
	return r
}

func TestReporter_RepeatedSameInfraReason_EditsExistingComment(t *testing.T) {
	w := &dedupWriter{}
	r := NewReporter(w)
	r.now = func() time.Time { return time.Date(2026, 5, 2, 4, 48, 28, 0, time.UTC) }

	const reason = "workspace: worktree has uncommitted changes — refusing to reuse"

	// First failure: posts a fresh comment, captures id=1001.
	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		newInfraResult(reason), "sess-1", "worker-1", 0, 3, "", "", nil); err != nil {
		t.Fatalf("Report 1: %v", err)
	}
	if len(w.writes) != 1 || w.writes[0].commentID != 1001 {
		t.Fatalf("first failure: writes=%+v", w.writes)
	}
	if len(w.edits) != 0 {
		t.Fatalf("first failure should not edit; got %+v", w.edits)
	}

	// Repeat 1 (same reason within TTL): should EDIT the same id, no new write.
	r.now = func() time.Time { return time.Date(2026, 5, 2, 4, 48, 58, 0, time.UTC) }
	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		newInfraResult(reason), "sess-1", "worker-1", 1, 3, "", "", nil); err != nil {
		t.Fatalf("Report 2: %v", err)
	}
	if len(w.writes) != 1 {
		t.Fatalf("repeat must not post a new comment; writes=%d", len(w.writes))
	}
	if len(w.edits) != 1 || w.edits[0].commentID != 1001 {
		t.Fatalf("repeat must edit comment 1001; edits=%+v", w.edits)
	}
	if !strings.Contains(w.edits[0].body, "Repeat #2") {
		t.Fatalf("badge missing #2; body[:200]=%q", w.edits[0].body[:200])
	}

	// Repeat 2: edit count increments to #3, still one write total.
	r.now = func() time.Time { return time.Date(2026, 5, 2, 4, 49, 30, 0, time.UTC) }
	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		newInfraResult(reason), "sess-1", "worker-1", 2, 3, "", "", nil); err != nil {
		t.Fatalf("Report 3: %v", err)
	}
	if len(w.writes) != 1 || len(w.edits) != 2 {
		t.Fatalf("after 3 failures: writes=%d edits=%d (want 1/2)", len(w.writes), len(w.edits))
	}
	if !strings.Contains(w.edits[1].body, "Repeat #3") {
		t.Fatalf("badge missing #3; body[:200]=%q", w.edits[1].body[:200])
	}
}

func TestReporter_DifferentReason_PostsFreshComment(t *testing.T) {
	w := &dedupWriter{}
	r := NewReporter(w)
	r.now = func() time.Time { return time.Date(2026, 5, 2, 4, 48, 28, 0, time.UTC) }

	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		newInfraResult("reason A"), "sess-1", "worker-1", 0, 3, "", "", nil); err != nil {
		t.Fatalf("Report A: %v", err)
	}
	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		newInfraResult("reason B"), "sess-1", "worker-1", 1, 3, "", "", nil); err != nil {
		t.Fatalf("Report B: %v", err)
	}
	if len(w.writes) != 2 {
		t.Fatalf("different reasons must produce two comments; writes=%d edits=%d", len(w.writes), len(w.edits))
	}
	if len(w.edits) != 0 {
		t.Fatalf("no edits expected when reasons differ; got %+v", w.edits)
	}
}

func TestReporter_TTLExpiry_PostsFreshComment(t *testing.T) {
	w := &dedupWriter{}
	r := NewReporter(w)
	now := time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		newInfraResult("same reason"), "sess-1", "worker-1", 0, 3, "", "", nil); err != nil {
		t.Fatalf("Report 1: %v", err)
	}
	// Advance past TTL.
	now = now.Add(infraDedupTTL + time.Minute)
	r.now = func() time.Time { return now }
	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		newInfraResult("same reason"), "sess-1", "worker-1", 1, 3, "", "", nil); err != nil {
		t.Fatalf("Report 2: %v", err)
	}
	if len(w.writes) != 2 {
		t.Fatalf("post-TTL must post a fresh comment; writes=%d", len(w.writes))
	}
}

func TestReporter_NonInfraReportClearsState(t *testing.T) {
	w := &dedupWriter{}
	r := NewReporter(w)
	r.now = func() time.Time { return time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC) }

	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		newInfraResult("worktree dirty"), "sess-1", "worker-1", 0, 3, "", "", nil); err != nil {
		t.Fatalf("infra: %v", err)
	}
	// A successful non-infra report flushes the dedup state.
	success := &runtimepkg.Result{ExitCode: 0, LastMessage: "done"}
	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		success, "sess-1", "worker-1", 0, 3, "", "", nil); err != nil {
		t.Fatalf("success: %v", err)
	}
	// Same infra reason a moment later: state was cleared, so the
	// reporter must post a fresh comment.
	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		newInfraResult("worktree dirty"), "sess-1", "worker-1", 0, 3, "", "", nil); err != nil {
		t.Fatalf("infra-2: %v", err)
	}
	if len(w.writes) != 3 {
		t.Fatalf("expected 3 writes (infra, success, infra-fresh); got %d edits=%d", len(w.writes), len(w.edits))
	}
	if len(w.edits) != 0 {
		t.Fatalf("no edits expected after success cleared state; got %+v", w.edits)
	}
}

func TestReporter_DifferentAgentSameIssue_NotDeduped(t *testing.T) {
	w := &dedupWriter{}
	r := NewReporter(w)
	r.now = func() time.Time { return time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC) }

	if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
		newInfraResult("reason"), "sess-1", "worker-1", 0, 3, "", "", nil); err != nil {
		t.Fatalf("dev: %v", err)
	}
	if err := r.Report(context.Background(), "owner/repo", 58, "review-agent",
		newInfraResult("reason"), "sess-2", "worker-1", 0, 3, "", "", nil); err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(w.writes) != 2 {
		t.Fatalf("each agent must get its own comment thread; writes=%d", len(w.writes))
	}
}

// fallbackWriter implements only GHCommentWriter (no Editor capability).
// Reporter must still post all comments fresh — never crash, never edit.
type fallbackWriter struct {
	writes int
}

func (f *fallbackWriter) WriteComment(_ string, _ int, _ string) error {
	f.writes++
	return nil
}

func TestReporter_NoEditorCapability_PostsFreshEachTime(t *testing.T) {
	w := &fallbackWriter{}
	r := NewReporter(w)
	r.now = func() time.Time { return time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC) }
	for i := 0; i < 3; i++ {
		if err := r.Report(context.Background(), "owner/repo", 58, "dev-agent",
			newInfraResult("same reason"), "sess-1", "worker-1", i, 3, "", "", nil); err != nil {
			t.Fatalf("Report %d: %v", i, err)
		}
	}
	if w.writes != 3 {
		t.Fatalf("fallback writer must post all 3 comments; got %d", w.writes)
	}
}
