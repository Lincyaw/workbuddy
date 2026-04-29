package reporter

import (
	"context"
	"strings"
	"testing"
	"time"
)

type recordingCommentWriter struct {
	repo  string
	issue int
	body  string
}

func (r *recordingCommentWriter) WriteComment(repo string, issueNum int, body string) error {
	r.repo = repo
	r.issue = issueNum
	r.body = body
	return nil
}

type fakeCycleCapTrailLoader struct {
	trail  []CycleRejectionEntry
	prURL  string
	branch string
	err    error
}

func (f *fakeCycleCapTrailLoader) LoadCycleCapTrail(_ context.Context, _ string, _ int) ([]CycleRejectionEntry, string, string, error) {
	return f.trail, f.prURL, f.branch, f.err
}

// TestFormatCycleCapReportContainsKeyFacts: the assembled comment includes the
// cycle/cap counts, workflow name, and each rejection-trail entry. AC-085-3.
func TestFormatCycleCapReportContainsKeyFacts(t *testing.T) {
	trail := []CycleRejectionEntry{
		{Timestamp: time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC), Summary: "review-agent rejected (exit=1)"},
		{Timestamp: time.Date(2026, 4, 29, 13, 0, 0, 0, time.UTC), Summary: "review-agent rejected (exit=1)"},
	}
	body := FormatCycleCapReport(CycleCapData{
		WorkflowName:    "default",
		CycleCount:      3,
		MaxReviewCycles: 3,
		HitAt:           time.Date(2026, 4, 29, 14, 30, 0, 0, time.UTC),
		RejectionTrail:  trail,
	})

	for _, want := range []string{
		"Dev↔Review Cycle Cap Reached",
		"3-cycle dev↔review cap",
		"`default`",
		"Cycle count",
		"`3`",
		"2026-04-29T14:30:00Z",
		"2026-04-29T12:00:00Z",
		"2026-04-29T13:00:00Z",
		"review-agent rejected",
		"Required action",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment body missing %q\n--body--\n%s", want, body)
		}
	}
}

// TestReportDevReviewCycleCapPostsCommentViaLoader: the Reporter consults the
// trail loader and posts the assembled body. AC-085-2 + AC-085-3.
func TestReportDevReviewCycleCapPostsCommentViaLoader(t *testing.T) {
	gh := &recordingCommentWriter{}
	rep := NewReporter(gh)
	rep.SetCycleCapTrailLoader(&fakeCycleCapTrailLoader{
		trail: []CycleRejectionEntry{
			{Timestamp: time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC), Summary: "review-agent rejected (exit=1)"},
		},
		prURL:  "https://github.com/owner/repo/pull/12",
		branch: "workbuddy/issue-9",
	})

	hitAt := time.Date(2026, 4, 29, 14, 30, 0, 0, time.UTC)
	if err := rep.ReportDevReviewCycleCap(context.Background(), "owner/repo", 9, "default", 3, 3, hitAt); err != nil {
		t.Fatalf("ReportDevReviewCycleCap: %v", err)
	}
	if gh.repo != "owner/repo" || gh.issue != 9 {
		t.Fatalf("comment routed to %s#%d", gh.repo, gh.issue)
	}
	for _, want := range []string{
		"Dev↔Review Cycle Cap Reached",
		"https://github.com/owner/repo/pull/12",
		"workbuddy/issue-9",
		"review-agent rejected",
	} {
		if !strings.Contains(gh.body, want) {
			t.Errorf("posted body missing %q", want)
		}
	}
}

// TestReportDevReviewCycleCapWithLoaderErrorStillPosts: loader failure must
// not block the cap-hit notification — the comment is posted with an empty
// trail. AC-085-3 (digest is best-effort).
func TestReportDevReviewCycleCapWithLoaderErrorStillPosts(t *testing.T) {
	gh := &recordingCommentWriter{}
	rep := NewReporter(gh)
	rep.SetCycleCapTrailLoader(&fakeCycleCapTrailLoader{err: errFake})

	hitAt := time.Date(2026, 4, 29, 14, 30, 0, 0, time.UTC)
	if err := rep.ReportDevReviewCycleCap(context.Background(), "owner/repo", 9, "default", 3, 3, hitAt); err != nil {
		t.Fatalf("ReportDevReviewCycleCap: %v", err)
	}
	if !strings.Contains(gh.body, "Dev↔Review Cycle Cap Reached") {
		t.Fatalf("loader error suppressed comment: %s", gh.body)
	}
}

type fakeError struct{ msg string }

func (e fakeError) Error() string { return e.msg }

var errFake = fakeError{"loader broken"}
