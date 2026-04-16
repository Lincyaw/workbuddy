package dependency

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type fakeReader struct {
	details map[int]poller.IssueDetails
}

func (f *fakeReader) ListIssues(string) ([]poller.Issue, error) { return nil, nil }

func (f *fakeReader) ReadIssue(_ string, issueNum int) (poller.IssueDetails, error) {
	if detail, ok := f.details[issueNum]; ok {
		return detail, nil
	}
	return poller.IssueDetails{}, nil
}

type fakeReaderWithErrors struct {
	details map[int]poller.IssueDetails
	errors  map[int]error
}

func (f *fakeReaderWithErrors) ListIssues(string) ([]poller.Issue, error) {
	return nil, nil
}

func (f *fakeReaderWithErrors) ReadIssue(_ string, issueNum int) (poller.IssueDetails, error) {
	if err, ok := f.errors[issueNum]; ok && err != nil {
		return poller.IssueDetails{}, err
	}
	if detail, ok := f.details[issueNum]; ok {
		return detail, nil
	}
	return poller.IssueDetails{}, nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestParseDeclaration(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		deps     []string
		statuses []string
		hasBlock bool
	}{
		{
			name:     "local and fqdn normalized",
			body:     "```yaml\nworkbuddy:\n  depends_on:\n    - \"#12\"\n    - \"Lincyaw/workbuddy#9\"\n```",
			deps:     []string{"Lincyaw/workbuddy#12", "Lincyaw/workbuddy#9"},
			statuses: []string{store.DependencyStatusActive, store.DependencyStatusActive},
			hasBlock: true,
		},
		{
			name:     "cross repo unsupported",
			body:     "```yaml\nworkbuddy:\n  depends_on:\n    - \"other/repo#7\"\n```",
			deps:     []string{"other/repo#7"},
			statuses: []string{store.DependencyStatusUnsupportedCrossRepo},
			hasBlock: true,
		},
		{
			name:     "malformed ignored",
			body:     "```yaml\nworkbuddy:\n  depends_on: nope\n```",
			hasBlock: false,
		},
		{
			name:     "natural language ignored",
			body:     "depends on #12",
			hasBlock: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDeclaration("Lincyaw/workbuddy", tt.body)
			if got.HasBlock != tt.hasBlock {
				t.Fatalf("HasBlock=%v want %v", got.HasBlock, tt.hasBlock)
			}
			if len(got.Dependencies) != len(tt.deps) {
				t.Fatalf("len deps=%d want %d", len(got.Dependencies), len(tt.deps))
			}
			for i, dep := range got.Dependencies {
				if dep.Normalized != tt.deps[i] {
					t.Fatalf("dep[%d]=%q want %q", i, dep.Normalized, tt.deps[i])
				}
				if dep.Status != tt.statuses[i] {
					t.Fatalf("dep[%d] status=%q want %q", i, dep.Status, tt.statuses[i])
				}
			}
		})
	}
}

func TestResolverSkipsRateLimitedDependencyRead(t *testing.T) {
	st := newTestStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 3,
		Labels:   `["status:developing"]`,
		Body:     "```yaml\nworkbuddy:\n  depends_on:\n    - \"#2\"\n```",
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}

	reader := &fakeReaderWithErrors{
		errors: map[int]error{
			2: fmt.Errorf("HTTP 403: rate limit exceeded"),
		},
	}
	logger := eventlog.NewEventLogger(st)
	resolver := NewResolver(st, reader, logger, nil)

	unblocked, err := resolver.EvaluateOpenIssues(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("EvaluateOpenIssues: %v", err)
	}
	if len(unblocked) != 0 {
		t.Fatalf("did not expect any unblocked issues, got: %v", unblocked)
	}

	state, err := st.QueryIssueDependencyState("owner/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Verdict != store.DependencyVerdictNeedsHuman {
		t.Fatalf("expected needs_human verdict, got: %+v", state)
	}

	evs, err := logger.Query(eventlog.EventFilter{Repo: "owner/repo", IssueNum: 3, Type: eventlog.TypeRateLimit})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("expected rate limit event to be recorded")
	}
}

func TestResolverSkipsRateLimitedDependencyReadRedactsTokenInLog(t *testing.T) {
	st := newTestStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 3,
		Labels:   `["status:developing"]`,
		Body:     "```yaml\nworkbuddy:\n  depends_on:\n    - \"#2\"\n```",
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}

	reader := &fakeReaderWithErrors{
		errors: map[int]error{
			2: fmt.Errorf("HTTP 403: rate limit exceeded: token ghp_12345678901234567890"),
		},
	}

	var out bytes.Buffer
	oldOutput := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&out)
	log.SetFlags(log.LstdFlags)
	defer func() {
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
	}()

	resolver := NewResolver(st, reader, nil, nil)
	_, err := resolver.EvaluateOpenIssues(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("EvaluateOpenIssues: %v", err)
	}

	logged := out.String()
	if strings.Contains(logged, "ghp_12345678901234567890") {
		t.Fatalf("expected token to be redacted in log output: %q", logged)
	}
}

func TestDetectCycles(t *testing.T) {
	graph := map[int][]int{
		1: {2},
		2: {3},
		3: {1},
		4: {5},
	}
	cycles := detectCycles(graph)
	for _, node := range []int{1, 2, 3} {
		if len(cycles[node]) == 0 {
			t.Fatalf("node %d should be in cycle", node)
		}
	}
	if len(cycles[4]) != 0 {
		t.Fatalf("node 4 unexpectedly in cycle: %v", cycles[4])
	}
}

func TestBuildResolveResultVerdicts(t *testing.T) {
	openIssues := map[int]poller.Issue{
		1: {Number: 1, Labels: []string{"status:developing"}},
		2: {Number: 2, Labels: []string{"status:done"}},
		3: {Number: 3, Labels: []string{"status:failed"}},
		4: {Number: 4, Labels: []string{"status:developing", OverrideLabel}},
	}
	reader := &fakeReader{details: map[int]poller.IssueDetails{
		5: {Number: 5, State: "closed", ClosedByLinkedPR: true},
	}}
	closedCache := map[int]poller.IssueDetails{}

	tests := []struct {
		name  string
		issue poller.Issue
		decl  ParsedDeclaration
		cycle []string
		want  string
	}{
		{
			name:  "ready when dep done",
			issue: openIssues[1],
			decl:  ParsedDeclaration{Dependencies: []ParsedDependency{{Raw: "#2", Repo: "owner/repo", IssueNum: 2, Status: store.DependencyStatusActive, Normalized: "owner/repo#2"}}},
			want:  store.DependencyVerdictReady,
		},
		{
			name:  "blocked when dep open",
			issue: openIssues[1],
			decl:  ParsedDeclaration{Dependencies: []ParsedDependency{{Raw: "#3", Repo: "owner/repo", IssueNum: 3, Status: store.DependencyStatusActive, Normalized: "owner/repo#3"}}},
			want:  store.DependencyVerdictBlocked,
		},
		{
			name:  "override wins",
			issue: openIssues[4],
			decl:  ParsedDeclaration{Dependencies: []ParsedDependency{{Raw: "#3", Repo: "owner/repo", IssueNum: 3, Status: store.DependencyStatusActive, Normalized: "owner/repo#3"}}},
			want:  store.DependencyVerdictOverride,
		},
		{
			name:  "needs human on cycle",
			issue: openIssues[1],
			decl:  ParsedDeclaration{Dependencies: []ParsedDependency{{Raw: "#2", Repo: "owner/repo", IssueNum: 2, Status: store.DependencyStatusActive, Normalized: "owner/repo#2"}}},
			cycle: []string{"#1", "#2", "#1"},
			want:  store.DependencyVerdictNeedsHuman,
		},
		{
			name:  "ready when closed via pr",
			issue: openIssues[1],
			decl:  ParsedDeclaration{Dependencies: []ParsedDependency{{Raw: "#5", Repo: "owner/repo", IssueNum: 5, Status: store.DependencyStatusActive, Normalized: "owner/repo#5"}}},
			want:  store.DependencyVerdictReady,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildResolveResult("owner/repo", tt.issue, tt.decl, 1, tt.cycle, openIssues, closedCache, reader, nil)
			if result.State.Verdict != tt.want {
				t.Fatalf("verdict=%q want %q", result.State.Verdict, tt.want)
			}
		})
	}
}

func TestResolverEvaluateOpenIssuesPersistsVerdictIdempotently(t *testing.T) {
	st := newTestStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 2,
		Labels:   `["status:done"]`,
		Body:     "",
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 3,
		Labels:   `["status:developing"]`,
		Body:     "```yaml\nworkbuddy:\n  depends_on:\n    - \"#2\"\n```",
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}

	resolver := NewResolver(st, &fakeReader{}, eventlog.NewEventLogger(st), nil)
	if _, err := resolver.EvaluateOpenIssues(context.Background(), "owner/repo", 1); err != nil {
		t.Fatalf("EvaluateOpenIssues: %v", err)
	}
	state, err := st.QueryIssueDependencyState("owner/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Verdict != store.DependencyVerdictReady {
		t.Fatalf("state verdict=%v", state)
	}

	// Second pass on identical cache should leave verdict intact.
	if _, err := resolver.EvaluateOpenIssues(context.Background(), "owner/repo", 2); err != nil {
		t.Fatalf("EvaluateOpenIssues second run: %v", err)
	}
	state2, err := st.QueryIssueDependencyState("owner/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if state2 == nil || state2.Verdict != store.DependencyVerdictReady {
		t.Fatalf("state verdict after second eval=%v", state2)
	}

	// Dependency rows must be persisted.
	deps, err := st.ListIssueDependencies("owner/repo", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].DependsOnIssueNum != 2 {
		t.Fatalf("unexpected deps: %+v", deps)
	}
}

// Invalid dependency refs (unparseable repo/issue) must not be persisted to
// the issue_dependencies table — they only surface in the verdict.
func TestResolverEvaluateSkipsInvalidRefsInDB(t *testing.T) {
	st := newTestStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 7,
		Labels:   `["status:developing"]`,
		Body:     "```yaml\nworkbuddy:\n  depends_on:\n    - \"not-a-ref\"\n```",
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver(st, &fakeReader{}, eventlog.NewEventLogger(st), nil)
	if _, err := resolver.EvaluateOpenIssues(context.Background(), "owner/repo", 1); err != nil {
		t.Fatalf("EvaluateOpenIssues: %v", err)
	}
	deps, err := st.ListIssueDependencies("owner/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Fatalf("invalid refs should not be persisted, got: %+v", deps)
	}
	state, err := st.QueryIssueDependencyState("owner/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Verdict != store.DependencyVerdictNeedsHuman {
		t.Fatalf("invalid ref should yield needs_human verdict, got: %+v", state)
	}
}
