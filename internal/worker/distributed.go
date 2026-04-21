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
)

type DistributedIssueReader interface {
	IssueLabelReader
	ReadIssue(repo string, issueNum int) (poller.IssueDetails, error)
}

type RemoteTaskClient interface {
	Heartbeat(ctx context.Context, taskID string, req workerclient.HeartbeatRequest) error
	ReleaseTask(ctx context.Context, taskID string, req workerclient.ReleaseRequest) error
	SubmitResult(ctx context.Context, taskID string, req workerclient.ResultRequest) error
}

type RemoteWorkspaceManager interface {
	Create(issueNum int, taskID string) (string, error)
	Remove(worktreePath string) error
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
	WorkspaceManager  RemoteWorkspaceManager
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
	sessionID := launchCtx.Session.ID
	reportWorkDir := w.deps.WorkDir

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

	var worktreePath string
	if w.deps.WorkspaceManager != nil {
		wt, err := w.deps.WorkspaceManager.Create(task.IssueNum, task.TaskID)
		if err != nil {
			log.Printf("[worker] failed to create worktree for issue #%d: %v", task.IssueNum, err)
			result := &runtimepkg.Result{ExitCode: 1, Stderr: err.Error()}
			reportCtx, cancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
			if rerr := w.deps.Reporter.Report(reportCtx, task.Repo, task.IssueNum, task.AgentName, result, sessionID, w.deps.WorkerID, 0, workflowMaxRetries(w.deps.Config, task.Workflow), "", ""); rerr != nil {
				log.Printf("[worker] failed to report worktree failure: %v", rerr)
			}
			cancel()
			releaseCtx, cancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
			if rerr := w.deps.Client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{WorkerID: w.deps.WorkerID, Reason: fmt.Sprintf("worktree setup failed: %v", err)}); rerr != nil {
				log.Printf("[worker] failed to release task after worktree setup failure: %v", rerr)
			}
			cancel()
			return fmt.Errorf("worker: worktree setup failed for issue #%d: %w", task.IssueNum, err)
		}
		reportWorkDir = wt
		worktreePath = wt
		launchCtx.RepoRoot = wt
		launchCtx.WorkDir = wt
		log.Printf("[worker] using worktree %s for issue #%d", wt, task.IssueNum)
	}
	if err := w.deps.Reporter.ReportStarted(taskCtx, task.Repo, task.IssueNum, task.AgentName, sessionID, w.deps.WorkerID); err != nil {
		log.Printf("[worker] report started failed: %v", err)
	}

	var cleanup func() error
	if worktreePath != "" && w.deps.WorkspaceManager != nil {
		cleanup = func() error { return w.deps.WorkspaceManager.Remove(worktreePath) }
	}
	execution := w.deps.Executor.Execute(taskCtx, Task{
		TaskID:            task.TaskID,
		Repo:              task.Repo,
		IssueNum:          task.IssueNum,
		AgentName:         task.AgentName,
		Agent:             &agentCopy,
		Context:           launchCtx,
		Workflow:          task.Workflow,
		WorkerID:          w.deps.WorkerID,
		Cleanup:           cleanup,
		EventDrainTimeout: postSessionDrainTimeout,
		RunSession: func(runCtx context.Context, session runtimepkg.Session, eventsCh chan<- launcherevents.Event) (*runtimepkg.Result, error) {
			return runRemoteSessionWithWatchdog(runCtx, session, eventsCh, w.deps.Config.Worker.StaleInference, task.TaskID, taskCancel)
		},
	})
	stopHeartbeat()
	result := execution.Result
	if w.deps.Recorder != nil {
		if err := w.deps.Recorder.Capture(sessionID, task.TaskID, task.Repo, task.IssueNum, task.AgentName, result); err != nil {
			log.Printf("[worker] audit capture failed: %v", err)
		}
	}
	if ctx.Err() != nil && !released.Load() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), w.deps.ShutdownTimeout)
		err := w.deps.Client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{WorkerID: w.deps.WorkerID, Reason: "worker shutdown"})
		cancel()
		if err == nil {
			released.Store(true)
		}
	}
	if released.Load() {
		return nil
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

	submitCtx, cancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
	defer cancel()
	if err := w.deps.Client.SubmitResult(submitCtx, task.TaskID, workerclient.ResultRequest{
		WorkerID:      w.deps.WorkerID,
		Status:        status,
		CurrentLabels: currentLabels,
		InfraFailure:  execution.InfraFailure(),
		InfraReason:   execution.InfraReason(),
	}); err != nil {
		log.Printf("[worker] submit result failed for task %s: %v", task.TaskID, err)
		return nil
	}
	reportCtx, reportCancel := context.WithTimeout(context.Background(), boundedRemoteTaskAPITimeout(w.deps.ShutdownTimeout))
	defer reportCancel()
	if err := w.deps.Reporter.ReportVerified(reportCtx, task.Repo, task.IssueNum, task.AgentName, result, sessionID, w.deps.WorkerID, 0, workflowMaxRetries(w.deps.Config, task.Workflow), "", reportWorkDir, verifyRes); err != nil {
		log.Printf("[worker] report failed: %v", err)
	}
	return nil
}

func BuildRemoteTaskContext(task *workerclient.Task, reader DistributedIssueReader, workDir string) *runtimepkg.TaskContext {
	issue := runtimepkg.IssueContext{Number: task.IssueNum}
	if reader != nil {
		if details, err := reader.ReadIssue(task.Repo, task.IssueNum); err == nil {
			issue.Body = details.Body
			issue.Labels = append([]string(nil), details.Labels...)
		}
	}
	return &runtimepkg.TaskContext{Issue: issue, Repo: task.Repo, RepoRoot: workDir, WorkDir: workDir, Session: runtimepkg.SessionContext{ID: fmt.Sprintf("session-%s", task.TaskID)}}
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

func workflowMaxRetries(cfg *config.FullConfig, workflow string) int {
	if cfg == nil || cfg.Workflows == nil {
		return 3
	}
	if wf, ok := cfg.Workflows[workflow]; ok && wf.MaxRetries > 0 {
		return wf.MaxRetries
	}
	return 3
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
