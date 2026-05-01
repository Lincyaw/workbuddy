package dependency

import (
	"context"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type testEventRecord struct {
	EventType string
	Repo      string
	IssueNum  int
	Payload   any
}

type testEventRecorder struct {
	records []testEventRecord
}

func (r *testEventRecorder) Log(eventType, repo string, issueNum int, payload interface{}) {
	r.records = append(r.records, testEventRecord{
		EventType: eventType,
		Repo:      repo,
		IssueNum:  issueNum,
		Payload:   payload,
	})
}

func (r *testEventRecorder) find(eventType string) []testEventRecord {
	var out []testEventRecord
	for _, record := range r.records {
		if record.EventType == eventType {
			out = append(out, record)
		}
	}
	return out
}

type noopIssueReader struct{}

func (noopIssueReader) ListIssues(string) ([]poller.Issue, error) { return nil, nil }

func (noopIssueReader) ReadIssue(string, int) (poller.IssueDetails, error) {
	return poller.IssueDetails{}, nil
}

func TestNormalizeDependencyHandlesEscapedQuotes(t *testing.T) {
	repo := "Lincyaw/workbuddy"
	tests := []struct {
		name      string
		raw       string
		wantRepo  string
		wantIssue int
		wantState string
		wantErr   string
	}{
		{
			name:      "backslash quoted local ref",
			raw:       `\"#292\"`,
			wantRepo:  repo,
			wantIssue: 292,
			wantState: store.DependencyStatusActive,
		},
		{
			name:      "quoted local ref",
			raw:       `"#292"`,
			wantRepo:  repo,
			wantIssue: 292,
			wantState: store.DependencyStatusActive,
		},
		{
			name:      "bare local ref",
			raw:       `#292`,
			wantRepo:  repo,
			wantIssue: 292,
			wantState: store.DependencyStatusActive,
		},
		{
			name:      "local ref must match entire shape",
			raw:       `#292garbage`,
			wantState: store.DependencyStatusInvalid,
			wantErr:   "invalid_format",
		},
		{
			name:      "fully qualified same repo ref",
			raw:       `Lincyaw/workbuddy#292`,
			wantRepo:  repo,
			wantIssue: 292,
			wantState: store.DependencyStatusActive,
		},
		{
			name:      "garbage is invalid",
			raw:       `garbage`,
			wantState: store.DependencyStatusInvalid,
			wantErr:   "invalid_format",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeDependency(repo, tc.raw)
			if got.Repo != tc.wantRepo || got.IssueNum != tc.wantIssue {
				t.Fatalf("normalizeDependency(%q) = repo=%q issue=%d, want repo=%q issue=%d", tc.raw, got.Repo, got.IssueNum, tc.wantRepo, tc.wantIssue)
			}
			if got.Status != tc.wantState {
				t.Fatalf("normalizeDependency(%q) status = %q, want %q", tc.raw, got.Status, tc.wantState)
			}
			if got.ParseErrorReason != tc.wantErr {
				t.Fatalf("normalizeDependency(%q) parse error = %q, want %q", tc.raw, got.ParseErrorReason, tc.wantErr)
			}
		})
	}
}

func TestEvaluateOpenIssues_LogsMalformedDependencyHazard(t *testing.T) {
	st, err := store.NewStore(t.TempDir() + "/dependency.db")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	const (
		repo     = "Lincyaw/workbuddy"
		issueNum = 293
	)

	body := "Prelude\n\n```yaml\nworkbuddy:\n  depends_on:\n    - \\\"#292\\\"\n    - garbage\n```\n"
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		Body:     body,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}

	rec := &testEventRecorder{}
	resolver := NewResolver(st, noopIssueReader{}, rec, nil)
	if _, err := resolver.EvaluateOpenIssues(context.Background(), repo, 1); err != nil {
		t.Fatalf("EvaluateOpenIssues: %v", err)
	}

	hazard, err := st.QueryIssuePipelineHazard(repo, issueNum)
	if err != nil {
		t.Fatalf("QueryIssuePipelineHazard: %v", err)
	}
	if hazard == nil || hazard.Kind != store.HazardKindMalformedDependencyRef {
		t.Fatalf("hazard = %+v, want malformed dependency ref hazard", hazard)
	}

	got := rec.find(eventlog.TypeIssueDependencyUnentered)
	if len(got) != 1 {
		t.Fatalf("expected 1 issue_dependency_unentered event, got %d", len(got))
	}
	payload, _ := got[0].Payload.(map[string]any)
	if payload["reason"] != "unparseable_ref" {
		t.Fatalf("reason = %v, want unparseable_ref", payload["reason"])
	}
	invalid, _ := payload["invalid_dependencies"].([]map[string]any)
	if len(invalid) != 1 {
		t.Fatalf("invalid_dependencies = %#v, want 1 entry", payload["invalid_dependencies"])
	}
	if invalid[0]["raw"] != "garbage" {
		t.Fatalf("invalid dependency raw = %v, want garbage", invalid[0]["raw"])
	}
	if invalid[0]["parse_error_reason"] != "invalid_format" {
		t.Fatalf("invalid dependency parse_error_reason = %v, want invalid_format", invalid[0]["parse_error_reason"])
	}
	if invalid[0]["line"] != 7 {
		t.Fatalf("invalid dependency line = %v, want 7", invalid[0]["line"])
	}

	deps, err := st.ListIssueDependencies(repo, issueNum)
	if err != nil {
		t.Fatalf("ListIssueDependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnIssueNum != 292 {
		t.Fatalf("persisted dependencies = %+v, want normalized #292 only", deps)
	}
}
