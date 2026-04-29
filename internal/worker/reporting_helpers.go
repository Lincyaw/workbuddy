package worker

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/labelcheck"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
	workersession "github.com/Lincyaw/workbuddy/internal/worker/session"
)

const defaultWorkerTaskAPITimeout = 5 * time.Second

func fetchCachedLabels(st *store.Store, repo string, issueNum int) []string {
	if st == nil {
		return nil
	}
	cached, err := st.QueryIssueCache(repo, issueNum)
	if err != nil || cached == nil {
		return nil
	}
	var labels []string
	if err := json.Unmarshal([]byte(cached.Labels), &labels); err != nil {
		log.Printf("[worker] failed to parse cached labels for %s#%d: %v", repo, issueNum, err)
		return nil
	}
	return labels
}

func recordLabelValidation(recorder *workersession.Recorder, sessionID, repo string, issueNum int, preLabels, postLabels []string, result *runtimepkg.Result, validation labelcheck.Result) {
	if recorder == nil {
		return
	}
	payload := audit.LabelValidationPayload{
		Pre:            cloneLabels(preLabels),
		Post:           cloneLabels(postLabels),
		ExitCode:       exitCodeForValidation(result),
		Classification: string(validation.Classification),
	}
	if err := recorder.RecordLabelValidationSession(sessionID, repo, issueNum, payload); err != nil {
		log.Printf("[worker] label validation audit failed: %v", err)
	}
}

func validateLabelTransition(cfg *config.FullConfig, workflow, state string, preLabels, postLabels []string, result *runtimepkg.Result) (labelcheck.Result, bool, error) {
	if cfg == nil {
		return labelcheck.Result{}, false, fmt.Errorf("missing worker config")
	}

	wf, ok := cfg.Workflows[workflow]
	if !ok || wf == nil {
		return labelcheck.Result{}, false, fmt.Errorf("workflow %q not found", workflow)
	}
	queuedState, ok := wf.States[state]
	if !ok || queuedState == nil {
		return labelcheck.Result{}, false, fmt.Errorf("state %q not found in workflow %q", state, workflow)
	}

	input := labelcheck.Input{
		Pre:      cloneLabels(preLabels),
		Post:     cloneLabels(postLabels),
		ExitCode: exitCodeForValidation(result),
		Current:  labelcheck.State{Name: state, Label: queuedState.EnterLabel},
	}

	stateNames := make([]string, 0, len(wf.States))
	for name := range wf.States {
		stateNames = append(stateNames, name)
	}
	sort.Strings(stateNames)

	knownSeen := make(map[string]bool)
	for _, name := range stateNames {
		stateCfg := wf.States[name]
		if stateCfg == nil || stateCfg.EnterLabel == "" || knownSeen[stateCfg.EnterLabel] {
			continue
		}
		knownSeen[stateCfg.EnterLabel] = true
		input.KnownStates = append(input.KnownStates, labelcheck.State{Name: name, Label: stateCfg.EnterLabel})
	}

	input.Current = labelcheck.ResolveCurrent(input.Pre, input.Current, input.KnownStates)
	currentState, err := resolveWorkflowLabelState(wf, input.Current)
	if err != nil {
		return labelcheck.Result{}, false, err
	}

	allowedSeen := make(map[string]bool)
	for _, targetName := range currentState.Transitions {
		target, ok := wf.States[targetName]
		if !ok || target == nil || target.EnterLabel == "" || allowedSeen[target.EnterLabel] {
			continue
		}
		allowedSeen[target.EnterLabel] = true
		input.AllowedTransitions = append(input.AllowedTransitions, labelcheck.State{Name: targetName, Label: target.EnterLabel})
	}

	return labelcheck.Classify(input), true, nil
}

func resolveWorkflowLabelState(wf *config.WorkflowConfig, current labelcheck.State) (*config.State, error) {
	if wf == nil {
		return nil, fmt.Errorf("missing workflow")
	}
	if current.Name != "" {
		if state, ok := wf.States[current.Name]; ok && state != nil {
			return state, nil
		}
	}
	if current.Label != "" {
		stateNames := make([]string, 0, len(wf.States))
		for name := range wf.States {
			stateNames = append(stateNames, name)
		}
		sort.Strings(stateNames)
		for _, name := range stateNames {
			stateCfg := wf.States[name]
			if stateCfg != nil && stateCfg.EnterLabel == current.Label {
				return stateCfg, nil
			}
		}
	}
	return nil, fmt.Errorf("resolved state %q (%q) not found in workflow %q", current.Name, current.Label, wf.Name)
}

func exitCodeForValidation(result *runtimepkg.Result) int {
	if result == nil {
		return -1
	}
	return result.ExitCode
}

func labelsUnchanged(pre, post []string) bool {
	if len(pre) != len(post) {
		return false
	}
	a := append([]string(nil), pre...)
	b := append([]string(nil), post...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func workflowMaxRetries(cfg *config.FullConfig, workflow string) int {
	if cfg == nil || cfg.Workflows == nil {
		return 3
	}
	if wf, ok := cfg.Workflows[workflow]; ok && wf.MaxRetries > 0 {
		return wf.MaxRetries
	}
	return 3
}

func boundedTaskAPITimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 || timeout > defaultWorkerTaskAPITimeout {
		return defaultWorkerTaskAPITimeout
	}
	return timeout
}
