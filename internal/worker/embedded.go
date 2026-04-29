package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/ghadapter"
	"github.com/Lincyaw/workbuddy/internal/labelcheck"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/router"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
	workersession "github.com/Lincyaw/workbuddy/internal/worker/session"
	"github.com/Lincyaw/workbuddy/internal/workspace"
)

const (
	defaultMaxParallelTasks = 4
	defaultTaskAPITimeout   = 5 * time.Second
	embeddedShutdownWait    = 60 * time.Second
)

// RunningTaskRegistry tracks per-issue cancel functions for active task runs.
type RunningTaskRegistry interface {
	Register(repo string, issue int, cancel context.CancelFunc)
	Remove(repo string, issue int)
}

// ClosedIssueTracker reports whether an issue was closed while work remained queued.
type ClosedIssueTracker interface {
	IsClosed(repo string, issue int) bool
}

// EmbeddedDeps bundles the runtime services needed by the embedded worker.
type EmbeddedDeps struct {
	Executor         *Executor
	Recorder         *workersession.Recorder
	Reporter         *reporter.Reporter
	Store            *store.Store
	StateMachine     *statemachine.StateMachine
	WorkerID         string
	Config           *config.FullConfig
	WorkspaceManager *workspace.Manager
	RunningTasks     RunningTaskRegistry
	ClosedIssues     ClosedIssueTracker
	TaskHub          *tasknotify.Hub
	IssueReader      IssueLabelReader
}

// EmbeddedWorker runs the in-process worker transport used by `workbuddy serve`.
type EmbeddedWorker struct {
	deps             EmbeddedDeps
	maxParallelTasks int
	issueLocks       issueTaskLocks
}

func NewEmbeddedWorker(deps EmbeddedDeps, maxParallelTasks int) *EmbeddedWorker {
	return &EmbeddedWorker{deps: deps, maxParallelTasks: maxParallelTasks}
}

func (w *EmbeddedWorker) Run(ctx context.Context, taskCh <-chan router.WorkerTask) {
	maxParallelTasks := w.maxParallelTasks
	if maxParallelTasks <= 0 {
		maxParallelTasks = defaultEmbeddedWorkerParallelism()
	}

	var wg sync.WaitGroup
	for i := 0; i < maxParallelTasks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverEmbeddedWorkerPanic("worker loop")
			for {
				select {
				case <-ctx.Done():
					return
				case task, ok := <-taskCh:
					if !ok {
						return
					}
					func() {
						defer func() {
							if recovered := recover(); recovered != nil {
								w.handleTaskPanic(task, recovered)
							}
						}()
						w.runWorkerTask(ctx, task)
					}()
				}
			}
		}()
	}
	wg.Wait()
}

func (w *EmbeddedWorker) ExecuteTask(ctx context.Context, task router.WorkerTask) {
	taskCtx := task.Context
	if taskCtx == nil {
		taskCtx = &runtimepkg.TaskContext{}
		task.Context = taskCtx
	}
	if taskCtx.Session.ID == "" && task.TaskID != "" {
		taskCtx.Session.ID = generateSessionID(task.TaskID)
	}

	// Create a per-task context that can be cancelled independently (e.g., on issue close).
	taskRunCtx, taskCancel := context.WithCancel(ctx)
	defer taskCancel()

	if w.deps.RunningTasks != nil {
		w.deps.RunningTasks.Register(task.Repo, task.IssueNum, taskCancel)
		defer w.deps.RunningTasks.Remove(task.Repo, task.IssueNum)
	}

	log.Printf("[worker] executing task %s: agent=%s issue=%s#%d",
		task.TaskID, task.AgentName, task.Repo, task.IssueNum)
	startedAt := time.Now().UTC()

	if w.deps.Store != nil {
		if err := w.deps.Store.UpdateTaskStatus(task.TaskID, store.TaskStatusRunning); err != nil {
			log.Printf("[worker] failed to update task status to running: %v", err)
		}
	}

	addClaimReaction(taskRunCtx, task.Repo, task.IssueNum, task.AgentName)

	sessionID := taskCtx.Session.ID
	taskCtx.Session.TaskID = task.TaskID
	taskCtx.Session.WorkerID = w.deps.WorkerID
	taskCtx.Session.Attempt = currentAttempt(task, w.deps.Store)

	var wsMgr WorkspaceManager
	if w.deps.WorkspaceManager != nil {
		wsMgr = w.deps.WorkspaceManager
	}
	execution := w.deps.Executor.Execute(taskRunCtx, Task{
		TaskID:           task.TaskID,
		Repo:             task.Repo,
		IssueNum:         task.IssueNum,
		AgentName:        task.AgentName,
		Agent:            task.Agent,
		Context:          task.Context,
		Workflow:         task.Workflow,
		State:            task.State,
		WorkerID:         w.deps.WorkerID,
		Attempt:          taskCtx.Session.Attempt,
		WorkspaceManager: wsMgr,
		OnPrepared: func(ctx context.Context, _ Task) {
			if w.deps.Reporter != nil {
				if err := w.deps.Reporter.ReportStarted(ctx, task.Repo, task.IssueNum, task.AgentName, sessionID, w.deps.WorkerID); err != nil {
					log.Printf("[worker] report started failed: %v", err)
				}
			}
		},
	})

	if execution.Result != nil && execution.Result.TokenUsage != nil && w.deps.Recorder != nil {
		if err := w.deps.Recorder.LogSession(sessionID, eventlog.TypeTokenUsage, task.Repo, task.IssueNum, execution.Result.TokenUsage); err != nil {
			log.Printf("[worker] token usage record failed: %v", err)
		}
	}
	if execution.RunErr != nil {
		log.Printf("[worker] agent %s failed: %v", task.AgentName, execution.RunErr)
	}

	completionLabels := execution.CompletionLabels
	if execution.PostSnapshotErr != nil {
		if cached := fetchCachedLabels(w.deps.Store, task.Repo, task.IssueNum); len(cached) > 0 {
			completionLabels = cached
		}
	}

	labelSummary := ""
	if execution.PreSnapshotErr == nil && execution.PostSnapshotErr == nil {
		if validation, ok, validationErr := validateLabelTransition(task, w.deps, execution.PreLabels, execution.PostLabels, execution.Result); validationErr != nil {
			log.Printf("[worker] label validation skipped: %v", validationErr)
		} else if ok {
			labelSummary = validation.Summary()
			payload := audit.LabelValidationPayload{
				Pre:            cloneLabels(execution.PreLabels),
				Post:           cloneLabels(execution.PostLabels),
				ExitCode:       exitCodeForValidation(execution.Result),
				Classification: string(validation.Classification),
			}
			if w.deps.Recorder != nil {
				if err := w.deps.Recorder.RecordLabelValidationSession(sessionID, task.Repo, task.IssueNum, payload); err != nil {
					log.Printf("[worker] label validation audit failed: %v", err)
				}
			}
			if validation.NeedsHumanRecommendation() && w.deps.Reporter != nil {
				if err := w.deps.Reporter.ReportNeedsHuman(taskRunCtx, task.Repo, task.IssueNum, labelSummary); err != nil {
					log.Printf("[worker] needs-human recommendation failed: %v", err)
				}
			}
		}
	} else if execution.PreSnapshotErr == nil || execution.PostSnapshotErr == nil {
		log.Printf("[worker] label validation skipped: incomplete label snapshots for %s#%d", task.Repo, task.IssueNum)
	} else {
		log.Printf("[worker] label validation skipped: label snapshots unavailable for %s#%d", task.Repo, task.IssueNum)
	}

	status := store.TaskStatusCompleted
	switch execution.Status() {
	case "timeout":
		status = store.TaskStatusTimeout
	case "failed":
		status = store.TaskStatusFailed
	}

	if w.deps.Store != nil {
		if err := w.deps.Store.UpdateTaskStatus(task.TaskID, status); err != nil {
			log.Printf("[worker] failed to update task status: %v", err)
		}
	}
	if w.deps.Recorder != nil {
		if err := w.deps.Recorder.Capture(sessionID, task.TaskID, task.Repo, task.IssueNum, task.AgentName, execution.Result); err != nil {
			log.Printf("[worker] audit capture failed: %v", err)
		}
	}

	if execution.InfraFailure() {
		source := execution.FailureSource
		if source == "" {
			source = "launcher_infra_failure"
		}
		publishTaskCompletion(w.deps.TaskHub, task, store.TaskStatusFailed, execution.ExitCode(), startedAt, execution.CompletedAt)
		logInfraFailureEvent(w.deps.Recorder, w.deps.Store, sessionID, task, execution.Result, source)
		if w.deps.Reporter != nil {
			reportCtx, reportCancel := context.WithTimeout(context.Background(), boundedTaskAPITimeout(embeddedShutdownWait))
			var reportWorkDir string
			if task.Context != nil {
				reportWorkDir = task.Context.WorkDir
			}
			if reportErr := w.deps.Reporter.Report(reportCtx, task.Repo, task.IssueNum, task.AgentName, execution.Result,
				sessionID, w.deps.WorkerID, 0, 3, labelSummary, reportWorkDir); reportErr != nil {
				log.Printf("[worker] infra-failure report failed: %v", reportErr)
			}
			reportCancel()
		}
		return
	}

	retryCount := 0
	maxRetries := 3
	if w.deps.Config != nil {
		if wf, ok := w.deps.Config.Workflows[task.Workflow]; ok && wf != nil {
			maxRetries = wf.MaxRetries
		}
	}
	if w.deps.Store != nil {
		counts, err := w.deps.Store.QueryTransitionCounts(task.Repo, task.IssueNum)
		if err == nil {
			for _, tc := range counts {
				if tc.ToState == task.State {
					retryCount = tc.Count
					break
				}
			}
		}
	}

	if w.deps.Reporter != nil {
		reportCtx, reportCancel := context.WithTimeout(context.Background(), boundedTaskAPITimeout(embeddedShutdownWait))
		defer reportCancel()
		var reportWorkDir string
		if task.Context != nil {
			reportWorkDir = task.Context.WorkDir
		}
		verifyResult, err := w.deps.Reporter.ReportWithVerification(reportCtx, task.Repo, task.IssueNum, task.AgentName, execution.Result,
			sessionID, w.deps.WorkerID, retryCount, maxRetries, labelSummary, reportWorkDir)
		if err != nil {
			log.Printf("[worker] report failed: %v", err)
		}

		exitCode := execution.ExitCode()
		if verifyResult != nil && verifyResult.Partial {
			exitCode = 1
			status = store.TaskStatusFailed
			if w.deps.Store != nil {
				if err := w.deps.Store.UpdateTaskStatus(task.TaskID, status); err != nil {
					log.Printf("[worker] failed to update task status after partial: %v", err)
				}
			}
		}

		if w.deps.StateMachine != nil {
			w.deps.StateMachine.MarkAgentCompleted(task.Repo, task.IssueNum, task.TaskID, task.AgentName, exitCode, completionLabels)
		}
		publishTaskCompletion(w.deps.TaskHub, task, status, exitCode, startedAt, execution.CompletedAt)

		if status == store.TaskStatusCompleted && execution.PreSnapshotErr == nil && execution.PostSnapshotErr == nil && labelsUnchanged(execution.PreLabels, execution.PostLabels) {
			log.Printf("[worker] agent %s completed for %s#%d but labels unchanged — redispatching", task.AgentName, task.Repo, task.IssueNum)
			if w.deps.Store != nil {
				if err := w.deps.Store.DeleteIssueCache(task.Repo, task.IssueNum); err != nil {
					log.Printf("[worker] fallback cache-invalidate failed: %v", err)
				}
			}
		}
		return
	}

	exitCode := execution.ExitCode()
	if w.deps.StateMachine != nil {
		w.deps.StateMachine.MarkAgentCompleted(task.Repo, task.IssueNum, task.TaskID, task.AgentName, exitCode, completionLabels)
	}
	publishTaskCompletion(w.deps.TaskHub, task, status, exitCode, startedAt, execution.CompletedAt)

	if status == store.TaskStatusCompleted && execution.PreSnapshotErr == nil && execution.PostSnapshotErr == nil && labelsUnchanged(execution.PreLabels, execution.PostLabels) {
		log.Printf("[worker] agent %s completed for %s#%d but labels unchanged — redispatching", task.AgentName, task.Repo, task.IssueNum)
		if w.deps.Store != nil {
			if err := w.deps.Store.DeleteIssueCache(task.Repo, task.IssueNum); err != nil {
				log.Printf("[worker] fallback cache-invalidate failed: %v", err)
			}
		}
	}
}

func (w *EmbeddedWorker) runWorkerTask(ctx context.Context, task router.WorkerTask) {
	issueLock := w.issueLocks.Acquire(task.Repo, task.IssueNum)
	defer issueLock.Release()

	if w.deps.ClosedIssues != nil && w.deps.ClosedIssues.IsClosed(task.Repo, task.IssueNum) {
		w.skipTaskForClosedIssue(task)
		return
	}

	w.ExecuteTask(ctx, task)
}

func (w *EmbeddedWorker) skipTaskForClosedIssue(task router.WorkerTask) {
	log.Printf("[worker] skipping queued task %s for closed issue %s#%d",
		task.TaskID, task.Repo, task.IssueNum)

	if w.deps.Store != nil {
		if err := w.deps.Store.UpdateTaskStatus(task.TaskID, store.TaskStatusFailed); err != nil {
			log.Printf("[worker] failed to update skipped task status: %v", err)
		}
	}
	if w.deps.StateMachine != nil {
		labels := fetchCachedLabels(w.deps.Store, task.Repo, task.IssueNum)
		w.deps.StateMachine.MarkAgentCompleted(task.Repo, task.IssueNum, task.TaskID, task.AgentName, 1, labels)
	}
}

func (w *EmbeddedWorker) handleTaskPanic(task router.WorkerTask, recovered any) {
	log.Printf("[worker] embedded worker recovered panic for task %s on %s#%d: %v", task.TaskID, task.Repo, task.IssueNum, recovered)
	if w.deps.Store != nil {
		if err := w.deps.Store.UpdateTaskStatus(task.TaskID, store.TaskStatusFailed); err != nil {
			log.Printf("[worker] failed to update panic task status: %v", err)
		}
	}
	panicResult := infraFailureResult(fmt.Sprintf("embedded worker panic: %v", recovered), fmt.Errorf("%v", recovered))
	sessionID := task.TaskID
	if task.Context != nil && task.Context.Session.ID != "" {
		sessionID = task.Context.Session.ID
	}
	logInfraFailureEvent(w.deps.Recorder, w.deps.Store, sessionID, task, panicResult, "embedded_worker_panic")
	now := time.Now().UTC()
	publishTaskCompletion(w.deps.TaskHub, task, store.TaskStatusFailed, panicResult.ExitCode, now, now)
}

func recoverEmbeddedWorkerPanic(scope string) {
	if recovered := recover(); recovered != nil {
		log.Printf("[worker] embedded worker recovered panic in %s: %v", scope, recovered)
	}
}

func defaultEmbeddedWorkerParallelism() int {
	if runtime.NumCPU() < defaultMaxParallelTasks {
		return runtime.NumCPU()
	}
	return defaultMaxParallelTasks
}

func currentAttempt(task router.WorkerTask, st *store.Store) int {
	if st == nil {
		return 0
	}
	counts, err := st.QueryTransitionCounts(task.Repo, task.IssueNum)
	if err != nil {
		return 0
	}
	for _, tc := range counts {
		if tc.ToState == task.State {
			return tc.Count
		}
	}
	return 0
}

func publishTaskCompletion(hub *tasknotify.Hub, task router.WorkerTask, status string, exitCode int, startedAt, completedAt time.Time) {
	if hub == nil {
		return
	}
	hub.Publish(tasknotify.TaskEvent{
		TaskID:      task.TaskID,
		Repo:        task.Repo,
		IssueNum:    task.IssueNum,
		AgentName:   task.AgentName,
		Status:      status,
		ExitCode:    exitCode,
		DurationMS:  completedAt.Sub(startedAt).Milliseconds(),
		StartedAt:   startedAt,
		CompletedAt: completedAt,
	})
}

func logInfraFailureEvent(recorder *workersession.Recorder, st *store.Store, sessionID string, task router.WorkerTask, result *runtimepkg.Result, source string) {
	if recorder == nil && st == nil {
		return
	}
	payload := map[string]any{
		"agent_name": task.AgentName,
		"task_id":    task.TaskID,
		"workflow":   task.Workflow,
		"state":      task.State,
		"source":     source,
	}
	if result != nil {
		payload["exit_code"] = result.ExitCode
		if result.Meta != nil {
			if reason := result.Meta[runtimepkg.MetaInfraFailureReason]; reason != "" {
				payload["reason"] = reason
			}
		}
		if result.Stderr != "" {
			stderr := result.Stderr
			if len(stderr) > 1024 {
				stderr = stderr[:1024]
			}
			payload["stderr_excerpt"] = stderr
		}
	}
	var err error
	if recorder != nil {
		err = recorder.RecordEventSession(sessionID, eventlog.TypeInfraFailure, task.Repo, task.IssueNum, payload)
	} else {
		err = workersession.RecordEvent(st, eventlog.TypeInfraFailure, task.Repo, task.IssueNum, payload)
	}
	if err != nil {
		log.Printf("[worker] failed to record infra failure event for %s#%d: %v", task.Repo, task.IssueNum, err)
	}
}

func addClaimReaction(ctx context.Context, repo string, issueNum int, agentName string) {
	if err := ghadapter.NewCLI().AddIssueReaction(ctx, repo, issueNum, "eyes"); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("[worker] failed to add claim reaction for %s on %s#%d: %v", agentName, repo, issueNum, err)
	}
}

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

func validateLabelTransition(task router.WorkerTask, deps EmbeddedDeps, preLabels, postLabels []string, result *runtimepkg.Result) (labelcheck.Result, bool, error) {
	if deps.Config == nil {
		return labelcheck.Result{}, false, fmt.Errorf("missing worker config")
	}

	wf, ok := deps.Config.Workflows[task.Workflow]
	if !ok || wf == nil {
		return labelcheck.Result{}, false, fmt.Errorf("workflow %q not found", task.Workflow)
	}
	queuedState, ok := wf.States[task.State]
	if !ok || queuedState == nil {
		return labelcheck.Result{}, false, fmt.Errorf("state %q not found in workflow %q", task.State, task.Workflow)
	}

	input := labelcheck.Input{
		Pre:      cloneLabels(preLabels),
		Post:     cloneLabels(postLabels),
		ExitCode: exitCodeForValidation(result),
		Current:  labelcheck.State{Name: task.State, Label: queuedState.EnterLabel},
	}

	stateNames := make([]string, 0, len(wf.States))
	for name := range wf.States {
		stateNames = append(stateNames, name)
	}
	sort.Strings(stateNames)

	knownSeen := make(map[string]bool)
	for _, name := range stateNames {
		state := wf.States[name]
		if state == nil || state.EnterLabel == "" || knownSeen[state.EnterLabel] {
			continue
		}
		knownSeen[state.EnterLabel] = true
		input.KnownStates = append(input.KnownStates, labelcheck.State{Name: name, Label: state.EnterLabel})
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
			state := wf.States[name]
			if state != nil && state.EnterLabel == current.Label {
				return state, nil
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

func boundedTaskAPITimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 || timeout > defaultTaskAPITimeout {
		return defaultTaskAPITimeout
	}
	return timeout
}
