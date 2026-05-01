package taskprep

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type fakeSynthesisReader struct {
	details map[int]struct {
		pr   runtimepkg.PRSummary
		diff string
	}
}

func (f fakeSynthesisReader) ReadIssueSummary(_ string, _ int) (string, string, []string, error) {
	return "Issue", "body", []string{"workbuddy"}, nil
}

func (f fakeSynthesisReader) ReadIssueComments(_ string, _ int) ([]runtimepkg.IssueComment, error) {
	return nil, nil
}

func (f fakeSynthesisReader) ListRelatedPRs(_ string, _ int) ([]runtimepkg.PRSummary, error) {
	return nil, nil
}

func (f fakeSynthesisReader) ReadPullRequestDetail(_ string, prNum int) (runtimepkg.PRSummary, string, error) {
	detail := f.details[prNum]
	return detail.pr, detail.diff, nil
}

func TestBuildSynthesisContext_UsesSourceStateJoinAndRolloutBranches(t *testing.T) {
	st := newTestStore(t)
	const (
		repo     = "test/repo"
		issue    = 293
		workflow = "default"
		groupID  = "rollout-group-293"
	)

	for i := 1; i <= 3; i++ {
		taskID := fmt.Sprintf("task-rollout-%d", i)
		if err := st.InsertTask(store.TaskRecord{
			ID:             taskID,
			Repo:           repo,
			IssueNum:       issue,
			AgentName:      "dev-agent",
			Workflow:       workflow,
			State:          "developing",
			RolloutIndex:   i,
			RolloutsTotal:  3,
			RolloutGroupID: groupID,
			Status:         store.TaskStatusCompleted,
		}); err != nil {
			t.Fatalf("InsertTask rollout %d: %v", i, err)
		}
		if _, err := st.InsertAgentSession(store.AgentSession{
			SessionID:  fmt.Sprintf("session-rollout-%d", i),
			TaskID:     taskID,
			Repo:       repo,
			IssueNum:   issue,
			AgentName:  "dev-agent",
			Summary:    fmt.Sprintf("summary for rollout %d", i),
			TaskStatus: store.TaskStatusCompleted,
		}); err != nil {
			t.Fatalf("InsertAgentSession rollout %d: %v", i, err)
		}
	}

	relatedPRs := []runtimepkg.PRSummary{
		{Number: 101, Title: "rollout 1", HeadRefName: "workbuddy/issue-293/rollout-1", HeadSHA: "sha-1", URL: "https://example/pr/101"},
		{Number: 102, Title: "rollout 2", HeadRefName: "workbuddy/issue-293/rollout-2", HeadSHA: "sha-2", URL: "https://example/pr/102"},
		{Number: 103, Title: "rollout 3", HeadRefName: "workbuddy/issue-293/rollout-3", HeadSHA: "sha-3", URL: "https://example/pr/103"},
	}
	gh := fakeSynthesisReader{
		details: map[int]struct {
			pr   runtimepkg.PRSummary
			diff string
		}{
			101: {pr: relatedPRs[0], diff: "diff --git a/a b/a\n+rollout1\n"},
			102: {pr: relatedPRs[1], diff: "diff --git a/b b/b\n+rollout2\n"},
			103: {pr: relatedPRs[2], diff: "diff --git a/c b/c\n+rollout3\n"},
		},
	}

	synthState := &config.State{EnterLabel: "status:synthesizing", Mode: config.StateModeSynth}
	sourceState := &config.State{
		EnterLabel: "status:developing",
		Join:       config.JoinConfig{Strategy: config.JoinRollouts, MinSuccesses: 2},
	}

	ctx, err := BuildSynthesisContext(repo, issue, workflow, "developing", synthState, sourceState, st, gh, relatedPRs)
	if err != nil {
		t.Fatalf("BuildSynthesisContext: %v", err)
	}
	if ctx == nil {
		t.Fatal("expected synthesis context")
	}
	if ctx.MinSuccesses != 2 {
		t.Fatalf("MinSuccesses = %d, want 2", ctx.MinSuccesses)
	}
	if len(ctx.Candidates) != 3 {
		t.Fatalf("Candidates = %d, want 3", len(ctx.Candidates))
	}

	for i, candidate := range ctx.Candidates {
		wantRollout := i + 1
		wantBranch := fmt.Sprintf("workbuddy/issue-293/rollout-%d", wantRollout)
		if candidate.RolloutIndex != wantRollout {
			t.Fatalf("candidate[%d].RolloutIndex = %d, want %d", i, candidate.RolloutIndex, wantRollout)
		}
		if candidate.PullRequest.HeadRefName != wantBranch {
			t.Fatalf("candidate[%d].HeadRefName = %q, want %q", i, candidate.PullRequest.HeadRefName, wantBranch)
		}
		if !strings.Contains(candidate.SessionSummary, "summary for rollout") {
			t.Fatalf("candidate[%d].SessionSummary = %q", i, candidate.SessionSummary)
		}
		if !strings.HasPrefix(candidate.SessionURL, "/sessions/session-rollout-") {
			t.Fatalf("candidate[%d].SessionURL = %q", i, candidate.SessionURL)
		}
		if !strings.Contains(candidate.Diff, "diff --git") {
			t.Fatalf("candidate[%d].Diff = %q", i, candidate.Diff)
		}
	}
}
