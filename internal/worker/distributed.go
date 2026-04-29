package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/staleinference"
	"github.com/Lincyaw/workbuddy/internal/store"
	workersession "github.com/Lincyaw/workbuddy/internal/worker/session"
	"github.com/Lincyaw/workbuddy/internal/workerclient"
)

const (
	defaultRemoteTaskAPITimeout = 5 * time.Second
	postSessionDrainTimeout     = 5 * time.Second

	// submitResultMaxAttempts bounds how many times a worker retries
	// SubmitResult against the coordinator before giving up and falling
	// back to an explicit ReleaseTask + GitHub breadcrumb. 3 attempts
	// with exponential backoff (250ms, 500ms, 1000ms) caps the total
	// wait below the usual lease/heartbeat window.
	submitResultMaxAttempts  = 3
	submitResultInitialDelay = 250 * time.Millisecond
	submitResultMaxDelay     = 2 * time.Second
)

// submitResultSleep is overridable from tests to avoid real backoff waits.
var submitResultSleep = func(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

type DistributedIssueReader interface {
	IssueLabelReader
	ReadIssue(repo string, issueNum int) (poller.IssueDetails, error)
}

type RemoteTaskClient interface {
	Heartbeat(ctx context.Context, taskID string, req workerclient.HeartbeatRequest) error
	ReleaseTask(ctx context.Context, taskID string, req workerclient.ReleaseRequest) error
	SubmitResult(ctx context.Context, taskID string, req workerclient.ResultRequest) error
}

type DistributedDeps struct {
	Config            *config.FullConfig
	Executor          *Executor
	Recorder          *workersession.Recorder
	Reporter          *reporter.Reporter
	Reader            DistributedIssueReader
	Client            RemoteTaskClient
	WorkDir           string
	WorkerID          string
	RuntimeAlias      string
	HeartbeatInterval time.Duration
	ShutdownTimeout   time.Duration
	WorkspaceManager  WorkspaceManager
}

type DistributedWorker struct {
	deps DistributedDeps
}

func NewDistributedWorker(deps DistributedDeps) *DistributedWorker {
	return &DistributedWorker{deps: deps}
}

func (w *DistributedWorker) ExecuteTask(ctx context.Context, task *workerclient.Task) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("distributed worker panic for task %s: %v", task.TaskID, recovered)
		}
	}()
	if task == nil {
		return fmt.Errorf("worker: task is required")
	}
	agentCfg, ok := w.deps.Config.Agents[task.AgentName]
	if !ok {
		return fmt.Errorf("worker: agent %q not found in local config", task.AgentName)
	}
	if w.deps.Executor == nil {
		return fmt.Errorf("worker: executor is required")
	}
	if w.deps.Client == nil {
		return fmt.Errorf("worker: remote client is required")
	}
	if w.deps.Reporter == nil {
		return fmt.Errorf("worker: reporter is required")
	}

	agentCopy := *agentCfg
	if w.deps.RuntimeAlias != "" {
		agentCopy.Runtime = w.deps.RuntimeAlias
	}

	launchCtx := BuildRemoteTaskContext(task, w.deps.Reader, w.deps.WorkDir)
	// Attach workflow-state metadata so the runtime can synthesize the
	// transition footer at prompt-render time. The remote Task does not carry
	// the State definition over the wire, so the worker resolves it locally
	// from its own config (issue #204 batch 3).
	if w.deps.Config != nil {
		if wf, ok := w.deps.Config.Workflows[task.Workflow]; ok && wf != nil {
			if state, ok := wf.States[task.State]; ok && state != nil {
				launchCtx.SetWorkflowState(task.State, state.EnterLabel, state.Transitions)
			} else {
				launchCtx.SetWorkflowState(task.State, "", nil)
			}
		}
	}
	sessionID := launchCtx.Session.ID

	taskCtx, taskCancel := context.WithCancel(ctx)
	defer taskCancel()

	var released atomic.Bool
	releaseDone := make(chan struct{})
	go func() {
		defer recoverDistributedPanic("shutdown release")
		select {
		case <-ctx.Done():
			releaseCtx, cancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
			defer cancel()
			if err := w.deps.Client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{WorkerID: w.deps.WorkerID, Reason: "worker shutdown"}); err == nil {
				released.Store(true)
			}
			taskCancel()
		case <-releaseDone:
		}
	}()
	defer close(releaseDone)

	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	var heartbeatStopOnce sync.Once
	stopHeartbeat := func() {
		heartbeatStopOnce.Do(func() { close(heartbeatStop) })
		<-heartbeatDone
	}
	go func() {
		defer close(heartbeatDone)
		defer recoverDistributedPanic("heartbeat loop")
		ticker := time.NewTicker(w.deps.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				hbCtx, cancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.HeartbeatInterval))
				err := w.deps.Client.Heartbeat(hbCtx, task.TaskID, workerclient.HeartbeatRequest{WorkerID: w.deps.WorkerID})
				cancel()
				if err != nil && !errorsIsContextCanceled(err) && !errorsIsUnauthorized(err) {
					if IsTaskOwnershipLost(err) {
						log.Printf("[worker] heartbeat reports ownership lost for task %s: %v — cancelling local execution", task.TaskID, err)
						taskCancel()
						return
					}
					log.Printf("[worker] heartbeat failed for task %s: %v", task.TaskID, err)
				}
			case <-heartbeatStop:
				return
			case <-taskCtx.Done():
				return
			}
		}
	}()
	defer stopHeartbeat()
	execution := w.deps.Executor.Execute(taskCtx, Task{
		TaskID:           task.TaskID,
		Repo:             task.Repo,
		IssueNum:         task.IssueNum,
		AgentName:        task.AgentName,
		Agent:            &agentCopy,
		Context:          launchCtx,
		Workflow:         task.Workflow,
		WorkerID:         w.deps.WorkerID,
		WorkspaceManager: w.deps.WorkspaceManager,
		OnPrepared: func(ctx context.Context, _ Task) {
			if err := w.deps.Reporter.ReportStarted(ctx, task.Repo, task.IssueNum, task.AgentName, sessionID, w.deps.WorkerID); err != nil {
				log.Printf("[worker] report started failed: %v", err)
			}
		},
		EventDrainTimeout: postSessionDrainTimeout,
		RunSession: func(runCtx context.Context, session runtimepkg.Session, eventsCh chan<- launcherevents.Event) (*runtimepkg.Result, error) {
			return runRemoteSessionWithWatchdog(runCtx, session, eventsCh, w.deps.Config.Worker.StaleInference, task.TaskID, taskCancel)
		},
	})
	stopHeartbeat()
	result := execution.Result
	reportWorkDir := w.deps.WorkDir
	if execution.Task.Context != nil && execution.Task.Context.WorkDir != "" {
		reportWorkDir = execution.Task.Context.WorkDir
	}
	if w.deps.Recorder != nil {
		if err := w.deps.Recorder.Capture(sessionID, task.TaskID, task.Repo, task.IssueNum, task.AgentName, result); err != nil {
			log.Printf("[worker] audit capture failed: %v", err)
		}
	}

	labelSummary := ""
	if execution.PreSnapshotErr == nil && execution.PostSnapshotErr == nil {
		if validation, ok, validationErr := validateLabelTransition(w.deps.Config, task.Workflow, task.State, execution.PreLabels, execution.PostLabels, result); validationErr != nil {
			log.Printf("[worker] label validation skipped: %v", validationErr)
		} else if ok {
			labelSummary = validation.Summary()
			recordLabelValidation(w.deps.Recorder, sessionID, task.Repo, task.IssueNum, execution.PreLabels, execution.PostLabels, result, validation)
			if validation.NeedsHumanRecommendation() {
				reportCtx, reportCancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
				if err := w.deps.Reporter.ReportNeedsHuman(reportCtx, task.Repo, task.IssueNum, labelSummary); err != nil {
					log.Printf("[worker] needs-human recommendation failed: %v", err)
				}
				reportCancel()
			}
		}
	} else if execution.PreSnapshotErr == nil || execution.PostSnapshotErr == nil {
		log.Printf("[worker] label validation skipped: incomplete label snapshots for %s#%d", task.Repo, task.IssueNum)
	} else {
		log.Printf("[worker] label validation skipped: label snapshots unavailable for %s#%d", task.Repo, task.IssueNum)
	}

	if ctx.Err() != nil && !released.Load() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
		err := w.deps.Client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{WorkerID: w.deps.WorkerID, Reason: "worker shutdown"})
		cancel()
		if err == nil {
			released.Store(true)
		}
	}
	if released.Load() {
		return nil
	}
	if execution.FailureSource == "worktree_setup_error" {
		reportCtx, reportCancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
		if err := w.deps.Reporter.Report(reportCtx, task.Repo, task.IssueNum, task.AgentName, result, sessionID, w.deps.WorkerID, 0, workflowMaxRetries(w.deps.Config, task.Workflow), labelSummary, "", nil); err != nil {
			log.Printf("[worker] failed to report worktree failure: %v", err)
		}
		reportCancel()

		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
		reason := fmt.Sprintf("worktree setup failed: %v", execution.RunErr)
		if result != nil && result.Meta != nil && result.Meta[runtimepkg.MetaInfraFailureReason] != "" {
			reason = result.Meta[runtimepkg.MetaInfraFailureReason]
		}
		if err := w.deps.Client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{WorkerID: w.deps.WorkerID, Reason: reason}); err != nil {
			log.Printf("[worker] failed to release task after worktree setup failure: %v", err)
		}
		releaseCancel()
		return execution.RunErr
	}

	currentLabels := execution.CompletionLabels
	if execution.PostSnapshotErr != nil {
		currentLabels = append([]string(nil), launchCtx.Issue.Labels...)
	}

	status := store.TaskStatusCompleted
	switch execution.Status() {
	case "failed":
		status = store.TaskStatusFailed
	case "timeout":
		status = store.TaskStatusTimeout
	}

	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
	verifyRes, verifyErr := w.deps.Reporter.Verify(verifyCtx, task.Repo, task.IssueNum, result)
	verifyCancel()
	if verifyErr != nil {
		log.Printf("[worker] claim verification error for task %s: %v", task.TaskID, verifyErr)
	}
	if verifyRes != nil && verifyRes.Partial {
		log.Printf("[worker] agent %s for %s#%d claimed side-effects not verified — treating as failure", task.AgentName, task.Repo, task.IssueNum)
		status = store.TaskStatusFailed
	}

	resultReq := workerclient.ResultRequest{
		WorkerID:      w.deps.WorkerID,
		Status:        status,
		CurrentLabels: currentLabels,
		InfraFailure:  execution.InfraFailure(),
		InfraReason:   execution.InfraReason(),
	}
	submitErr := w.submitResultWithRetry(ctx, task.TaskID, resultReq)
	if submitErr != nil {
		// Result delivery failed even after bounded retry. Treat as a
		// first-class failure: (1) still post a GitHub breadcrumb so
		// operators see an "agent completed but coordinator sync
		// failed" trail, (2) explicitly release the lease with a
		// useful reason so the coordinator knows it's free instead of
		// waiting for lease expiry, (3) propagate a non-nil error so
		// shutdown/cancel paths upstream can react.
		log.Printf("[worker] submit result permanently failed for task %s after %d attempts: %v", task.TaskID, submitResultMaxAttempts, submitErr)

		reportCtx, reportCancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
		if err := w.deps.Reporter.ReportVerified(reportCtx, task.Repo, task.IssueNum, task.AgentName, result, sessionID, w.deps.WorkerID, 0, workflowMaxRetries(w.deps.Config, task.Workflow), labelSummary, reportWorkDir, verifyRes, &reporter.SyncFailure{
			Operation: "submit_result",
			Detail:    submitErr.Error(),
		}); err != nil {
			log.Printf("[worker] report after submit-failure failed: %v", err)
		}
		reportCancel()

		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
		reason := fmt.Sprintf("submit result failed: %v", submitErr)
		if err := w.deps.Client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{WorkerID: w.deps.WorkerID, Reason: reason}); err != nil {
			log.Printf("[worker] release after submit-failure failed for task %s: %v", task.TaskID, err)
		} else {
			released.Store(true)
		}
		releaseCancel()

		return fmt.Errorf("submit result for task %s: %w", task.TaskID, submitErr)
	}

	reportCtx, reportCancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
	defer reportCancel()
	if err := w.deps.Reporter.ReportVerified(reportCtx, task.Repo, task.IssueNum, task.AgentName, result, sessionID, w.deps.WorkerID, 0, workflowMaxRetries(w.deps.Config, task.Workflow), labelSummary, reportWorkDir, verifyRes, nil); err != nil {
		log.Printf("[worker] report failed: %v", err)
	}
	return nil
}

// submitResultWithRetry attempts SubmitResult up to submitResultMaxAttempts
// times with exponential backoff, stopping early on unrecoverable errors
// (unauthorized, ownership-lost, context cancellation). Returns the final
// error or nil on success.
func (w *DistributedWorker) submitResultWithRetry(ctx context.Context, taskID string, req workerclient.ResultRequest) error {
	delay := submitResultInitialDelay
	var lastErr error
	for attempt := 1; attempt <= submitResultMaxAttempts; attempt++ {
		submitCtx, cancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
		err := w.deps.Client.SubmitResult(submitCtx, taskID, req)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		// Non-retryable conditions: parent context already dead, auth
		// rejected (creds wrong, not a transient blip), or coordinator
		// says we no longer own the task.
		if ctx.Err() != nil || errorsIsUnauthorized(err) || IsTaskOwnershipLost(err) {
			log.Printf("[worker] submit result for task %s non-retryable on attempt %d/%d: %v", taskID, attempt, submitResultMaxAttempts, err)
			return err
		}
		if attempt == submitResultMaxAttempts {
			break
		}
		log.Printf("[worker] submit result for task %s failed on attempt %d/%d: %v (retrying in %s)", taskID, attempt, submitResultMaxAttempts, err, delay)
		submitResultSleep(ctx, delay)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay *= 2
		if delay > submitResultMaxDelay {
			delay = submitResultMaxDelay
		}
	}
	return lastErr
}

func BuildRemoteTaskContext(task *workerclient.Task, reader DistributedIssueReader, workDir string) *runtimepkg.TaskContext {
	issue := runtimepkg.IssueContext{Number: task.IssueNum}
	if reader != nil {
		if details, err := reader.ReadIssue(task.Repo, task.IssueNum); err == nil {
			issue.Body = details.Body
			issue.Labels = append([]string(nil), details.Labels...)
		}
	}
	return &runtimepkg.TaskContext{Issue: issue, Repo: task.Repo, RepoRoot: workDir, WorkDir: workDir, Session: runtimepkg.SessionContext{ID: generateSessionID(task.TaskID)}}
}

func runRemoteSessionWithWatchdog(
	ctx context.Context,
	session runtimepkg.Session,
	eventsCh chan<- launcherevents.Event,
	siCfg config.StaleInferenceConfig,
	taskID string,
	taskCancel context.CancelFunc,
) (*runtimepkg.Result, error) {
	sessionCh := eventsCh
	var proxyDone chan struct{}
	if siCfg.StaleInferenceEnabled() {
		tracker := staleinference.NewEventTracker()
		watchdogCtx, watchdogCancel := context.WithCancel(ctx)
		defer watchdogCancel()
		go staleinference.Watch(watchdogCtx, staleinference.Config{IdleThreshold: siCfg.IdleThreshold, CheckInterval: siCfg.CheckInterval, CompletedGracePeriod: siCfg.CompletedGracePeriod}, tracker, taskCancel)

		proxyCh := make(chan launcherevents.Event, 64)
		proxyDone = make(chan struct{})
		go func() {
			defer close(proxyDone)
			defer recoverDistributedPanic("event watchdog proxy")
			for evt := range proxyCh {
				if evt.Kind == launcherevents.KindTaskComplete {
					tracker.RecordCompletion()
				} else {
					tracker.RecordActivity()
				}
				select {
				case eventsCh <- evt:
				case <-ctx.Done():
					for range proxyCh {
					}
					return
				}
			}
		}()
		sessionCh = proxyCh
	}

	result, runErr := session.Run(ctx, sessionCh)
	if proxyDone != nil {
		close(sessionCh)
		select {
		case <-proxyDone:
		case <-time.After(postSessionDrainTimeout):
			log.Printf("[worker] proxy drain timed out for task %s after %s; cancelling task ctx to unblock", taskID, postSessionDrainTimeout)
			taskCancel()
			<-proxyDone
		}
	}
	return result, runErr
}

func boundedRemoteTaskAPITimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 || timeout > defaultRemoteTaskAPITimeout {
		return defaultRemoteTaskAPITimeout
	}
	return timeout
}

func IsTaskOwnershipLost(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "worker_id does not match claimed task") ||
		strings.Contains(msg, "task already completed") ||
		strings.Contains(msg, "not claimable by this worker") ||
		strings.Contains(msg, "task is no longer owned by worker")
}

func recoverDistributedPanic(scope string) {
	if recovered := recover(); recovered != nil {
		log.Printf("[worker] distributed worker recovered panic in %s: %v", scope, recovered)
	}
}

func errorsIsContextCanceled(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func errorsIsUnauthorized(err error) bool {
	return errors.Is(err, workerclient.ErrUnauthorized)
}
