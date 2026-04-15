package labelcheck

import (
	"fmt"
	"slices"
)

type State struct {
	Name  string
	Label string
}

type Input struct {
	Pre                []string
	Post               []string
	ExitCode           int
	Current            State
	AllowedTransitions []State
	KnownStates        []State
}

type Classification string

const (
	ClassificationOK                       Classification = "ok"
	ClassificationNoTransitionAfterSuccess Classification = "no_transition_after_success"
	ClassificationNoTransitionAfterFailure Classification = "no_transition_after_failure"
	ClassificationUnexpectedTransition     Classification = "unexpected_transition"
	ClassificationFailed                   Classification = "failed"
)

type Result struct {
	Classification Classification
	From           State
	To             State
}

func Classify(input Input) Result {
	current := input.Current
	if preState := selectPrimaryState(matchStates(input.Pre, input.KnownStates), State{}); preState.Label != "" {
		current = preState
	}

	postStates := matchStates(input.Post, input.KnownStates)
	if failed, ok := stateNamed(postStates, "failed"); ok {
		return Result{
			Classification: ClassificationFailed,
			From:           current,
			To:             failed,
		}
	}

	switch {
	case len(postStates) == 0:
		if current.Label == "" {
			return Result{
				Classification: noTransitionClassification(input.ExitCode),
				From:           current,
			}
		}
		return Result{
			Classification: ClassificationUnexpectedTransition,
			From:           current,
		}
	case len(postStates) == 1 && current.Label != "" && postStates[0].Label == current.Label:
		return Result{
			Classification: noTransitionClassification(input.ExitCode),
			From:           current,
			To:             postStates[0],
		}
	case len(postStates) == 1 && containsState(input.AllowedTransitions, postStates[0]):
		return Result{
			Classification: ClassificationOK,
			From:           current,
			To:             postStates[0],
		}
	default:
		return Result{
			Classification: ClassificationUnexpectedTransition,
			From:           current,
			To:             selectPrimaryState(postStates, current),
		}
	}
}

func (r Result) Summary() string {
	from := displayState(r.From)
	to := displayState(r.To)

	switch r.Classification {
	case ClassificationOK:
		return fmt.Sprintf("Label transition: %s -> %s (OK)", from, to)
	case ClassificationNoTransitionAfterSuccess:
		return "Label transition: none - needs human review"
	case ClassificationNoTransitionAfterFailure:
		return "Label transition: none - retry path"
	case ClassificationFailed:
		return fmt.Sprintf("Label transition: %s -> %s (failed)", from, to)
	default:
		return fmt.Sprintf("Label transition: %s -> %s (unexpected)", from, to)
	}
}

func (r Result) NeedsHumanRecommendation() bool {
	return r.Classification == ClassificationNoTransitionAfterSuccess
}

func noTransitionClassification(exitCode int) Classification {
	if exitCode == 0 {
		return ClassificationNoTransitionAfterSuccess
	}
	return ClassificationNoTransitionAfterFailure
}

func matchStates(labels []string, known []State) []State {
	matches := make([]State, 0, len(known))
	for _, state := range known {
		if state.Label == "" {
			continue
		}
		if slices.Contains(labels, state.Label) {
			matches = append(matches, state)
		}
	}
	return matches
}

func containsState(states []State, want State) bool {
	for _, state := range states {
		if state.Label == want.Label && state.Name == want.Name {
			return true
		}
	}
	return false
}

func selectPrimaryState(states []State, current State) State {
	for _, state := range states {
		if current.Label != "" && state.Label != current.Label {
			return state
		}
	}
	if len(states) == 0 {
		return State{}
	}
	return states[0]
}

func stateNamed(states []State, name string) (State, bool) {
	for _, state := range states {
		if state.Name == name {
			return state, true
		}
	}
	return State{}, false
}

func displayState(state State) string {
	switch {
	case state.Name != "":
		return state.Name
	case state.Label != "":
		return state.Label
	default:
		return "none"
	}
}
