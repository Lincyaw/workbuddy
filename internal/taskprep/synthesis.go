package taskprep

import (
	"fmt"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type synthesisStore interface {
	LatestRolloutGroupSummaryForIssueState(repo string, issueNum int, workflow, state string) (*store.RolloutGroupSummary, error)
	ListAgentSessions(f store.SessionFilter) ([]store.AgentSession, error)
}

type pullRequestDetailReader interface {
	ReadPullRequestDetail(repo string, prNum int) (runtimepkg.PRSummary, string, error)
}

func BuildSynthesisContext(repo string, issueNum int, workflow, sourceState string, synthStateDef, sourceStateDef *config.State, st synthesisStore, gh IssueDataReader, relatedPRs []runtimepkg.PRSummary) (*runtimepkg.SynthesisContext, error) {
	if stateMode := stateMode(synthStateDef); stateMode != config.StateModeSynth {
		return nil, nil
	}
	minSuccesses := 0
	if sourceStateDef != nil {
		minSuccesses = sourceStateDef.Join.MinSuccesses
	}
	if minSuccesses <= 0 && synthStateDef != nil {
		minSuccesses = synthStateDef.Join.MinSuccesses
	}
	ctx := &runtimepkg.SynthesisContext{
		SourceState:  sourceState,
		MinSuccesses: minSuccesses,
	}
	if st == nil || strings.TrimSpace(sourceState) == "" {
		return ctx, nil
	}

	summary, err := st.LatestRolloutGroupSummaryForIssueState(repo, issueNum, workflow, sourceState)
	if err != nil || summary == nil {
		return ctx, err
	}
	sessionByTask, err := latestSessionByTask(st, repo, issueNum)
	if err != nil {
		return nil, err
	}
	prByBranch := make(map[string]runtimepkg.PRSummary, len(relatedPRs))
	for _, pr := range relatedPRs {
		prByBranch[pr.HeadRefName] = pr
	}
	detailReader, _ := gh.(pullRequestDetailReader)
	for _, task := range summary.Tasks {
		if task.RolloutIndex <= 0 {
			continue
		}
		branch := fmt.Sprintf("workbuddy/issue-%d/rollout-%d", issueNum, task.RolloutIndex)
		pr, ok := prByBranch[branch]
		if !ok {
			continue
		}
		diff := ""
		if detailReader != nil {
			if detailedPR, detailedDiff, detailErr := detailReader.ReadPullRequestDetail(repo, pr.Number); detailErr == nil {
				pr = detailedPR
				diff = detailedDiff
			}
		}
		candidate := runtimepkg.SynthesisCandidate{
			TaskID:       task.ID,
			RolloutIndex: task.RolloutIndex,
			PullRequest:  pr,
			Diff:         diff,
		}
		if session, ok := sessionByTask[task.ID]; ok {
			candidate.SessionSummary = strings.TrimSpace(session.Summary)
			if session.SessionID != "" {
				candidate.SessionURL = "/sessions/" + session.SessionID
			}
		}
		ctx.Candidates = append(ctx.Candidates, candidate)
	}
	return ctx, nil
}

func latestSessionByTask(st synthesisStore, repo string, issueNum int) (map[string]store.AgentSession, error) {
	sessions, err := st.ListAgentSessions(store.SessionFilter{Repo: repo, IssueNum: issueNum})
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.AgentSession)
	for _, session := range sessions {
		if strings.TrimSpace(session.TaskID) == "" {
			continue
		}
		if prev, ok := out[session.TaskID]; !ok || prev.CreatedAt.Before(session.CreatedAt) {
			out[session.TaskID] = session
		}
	}
	return out, nil
}

func stateMode(state *config.State) string {
	if state == nil || strings.TrimSpace(state.Mode) == "" {
		return config.StateModeReview
	}
	return state.Mode
}
