package control

import (
	"context"
)

// LoopConfig configures the control loop.
type LoopConfig struct {
	Observer   Observer
	Controller *Controller
	Repo       string
	IssueNum   int
	Branch     string
	Objective  *Objective
}

// LoopResult is the outcome of the control loop.
type LoopResult struct {
	Action  Action
	Rounds  int
	Message string   // final status message
	Deltas  []*Delta // history of observations
}

// RunAgent is the callback the control loop calls to execute (or resume) the
// agent. On the first round, resumeSessionID is empty and the caller should
// start a fresh session. On subsequent rounds, resumeSessionID carries the
// previous session ID so the agent can be resumed with the feedback prompt.
type RunAgent func(ctx context.Context, prompt string, resumeSessionID string) (sessionID string, err error)

// Run executes the control loop. It calls runAgent for each round, then
// observes the world state, computes the feedback delta, and decides whether
// to resume, block, or complete.
//
// First round uses an empty prompt (the original prompt comes from the
// caller's dispatch config). Subsequent rounds use the feedback message
// derived from the previous observation delta.
func Run(ctx context.Context, cfg *LoopConfig, runAgent RunAgent) (*LoopResult, error) {
	var prevDelta *Delta
	var sessionID string
	var deltas []*Delta

	for round := 0; ; round++ {
		var prompt string
		if round > 0 && prevDelta != nil {
			prompt = prevDelta.Message(cfg.Repo, cfg.IssueNum, cfg.Branch)
		}

		var err error
		if round == 0 {
			sessionID, err = runAgent(ctx, "", "")
		} else {
			sessionID, err = runAgent(ctx, prompt, sessionID)
		}
		if err != nil {
			return &LoopResult{
				Action:  ActionBlock,
				Rounds:  round + 1,
				Message: err.Error(),
				Deltas:  deltas,
			}, err
		}

		obs, err := cfg.Observer.Observe(ctx, cfg.Repo, cfg.IssueNum, cfg.Branch)
		if err != nil {
			return &LoopResult{
				Action:  ActionBlock,
				Rounds:  round + 1,
				Message: err.Error(),
				Deltas:  deltas,
			}, err
		}

		delta := Feedback(cfg.Objective, obs)
		deltas = append(deltas, delta)

		decision := cfg.Controller.Decide(delta, round+1, prevDelta)

		if decision.Action != ActionResume {
			return &LoopResult{
				Action:  decision.Action,
				Rounds:  round + 1,
				Message: decision.Message,
				Deltas:  deltas,
			}, nil
		}

		prevDelta = delta
	}
}
