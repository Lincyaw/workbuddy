package worker

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
	workersession "github.com/Lincyaw/workbuddy/internal/worker/session"
)

// IssueLabelReader reads current issue labels for pre/post execution snapshots.
type IssueLabelReader interface {
	ReadIssueLabels(repo string, issueNum int) ([]string, error)
}

// Executor owns the shared runtime/session execution lifecycle.
type Executor struct {
	launcher    *runtimepkg.Registry
	issueReader IssueLabelReader
	locks       issueTaskLocks
	mu          sync.Mutex
	lifecycle   context.Context
	stop        context.CancelFunc
}

func NewExecutor(lnch *runtimepkg.Registry, issueReader IssueLabelReader) *Executor {
	return &Executor{launcher: lnch, issueReader: issueReader}
}

// Start initializes the executor lifecycle context if it is not already running.
func (e *Executor) Start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lifecycle != nil && e.lifecycle.Err() == nil {
		return
	}
	e.lifecycle, e.stop = context.WithCancel(context.Background())
}

// Stop cancels the executor lifecycle so in-flight runs can drain and exit.
func (e *Executor) Stop() {
	e.mu.Lock()
	stop := e.stop
	e.lifecycle = nil
	e.stop = nil
	e.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (e *Executor) Execute(ctx context.Context, task Task) Execution {
	e.Start()
	ctx, cancelLifecycle := e.withLifecycleContext(ctx)
	defer cancelLifecycle()

	exec := Execution{Task: task}
	lock := e.locks.Acquire(task.Repo, task.IssueNum)
	defer lock.Release()

	taskCtx := task.Context
	if taskCtx == nil {
		taskCtx = &runtimepkg.TaskContext{}
	}
	if taskCtx.Session.ID == "" && task.TaskID != "" {
		taskCtx.Session.ID = fmt.Sprintf("session-%s", task.TaskID)
	}
	taskCtx.Repo = task.Repo
	taskCtx.Issue.Number = task.IssueNum
	taskCtx.Session.TaskID = task.TaskID
	taskCtx.Session.WorkerID = task.WorkerID
	taskCtx.Session.Attempt = task.Attempt
	task.Context = taskCtx
	exec.Task = task
	sessionStatus := store.TaskStatusFailed
	defer func() {
		if err := closeManagedSession(taskCtx, sessionStatus); err != nil {
			log.Printf("[worker] session finalize failed for %s#%d: %v", task.Repo, task.IssueNum, err)
		}
	}()

	if task.Cleanup != nil {
		defer func() {
			if err := task.Cleanup(); err != nil {
				log.Printf("[worker] cleanup failed for %s#%d: %v", task.Repo, task.IssueNum, err)
			}
		}()
	}

	exec.StartedAt = time.Now().UTC()
	session, err := e.launcher.Start(ctx, task.Agent, taskCtx)
	if err != nil {
		exec.CompletedAt = time.Now().UTC()
		exec.RunErr = err
		exec.Result = infraFailureResult("launcher Start() failed: "+err.Error(), err)
		exec.FailureSource = "launcher_start_error"
		sessionStatus = executionStatus(exec.Result, err)
		return exec
	}
	defer func() { _ = session.Close() }()

	preLabels, preErr := snapshotIssueLabels(task.Repo, task.IssueNum, e.issueReader)
	exec.PreLabels = preLabels
	exec.PreSnapshotErr = preErr
	if preErr == nil {
		taskCtx.Session.PreLabels = cloneLabels(preLabels)
	}

	eventsCh := make(chan launcherevents.Event, 64)
	eventsPath, waitEvents := workersession.Stream(taskCtx, eventsCh)
	runSession := task.RunSession
	if runSession == nil {
		runSession = func(ctx context.Context, session runtimepkg.Session, eventsCh chan<- launcherevents.Event) (*runtimepkg.Result, error) {
			return session.Run(ctx, eventsCh)
		}
	}
	result, runErr := runSession(ctx, session, eventsCh)
	close(eventsCh)
	if task.EventDrainTimeout > 0 {
		waitDone := make(chan error, 1)
		go func() { waitDone <- waitEvents() }()
		select {
		case exec.EventErr = <-waitDone:
		case <-time.After(task.EventDrainTimeout):
			exec.EventErr = fmt.Errorf("event stream drain timed out after %s", task.EventDrainTimeout)
		}
	} else {
		exec.EventErr = waitEvents()
	}
	if exec.EventErr != nil {
		log.Printf("[worker] event capture failed for %s#%d: %v", task.Repo, task.IssueNum, exec.EventErr)
	}

	if result != nil && eventsPath != "" && exec.EventErr == nil {
		if result.RawSessionPath == "" {
			result.RawSessionPath = result.SessionPath
		}
		result.SessionPath = eventsPath
	}

	postLabels, postErr := snapshotIssueLabels(task.Repo, task.IssueNum, e.issueReader)
	exec.PostLabels = postLabels
	exec.PostSnapshotErr = postErr
	if postErr == nil {
		taskCtx.Session.PostLabels = cloneLabels(postLabels)
		exec.CompletionLabels = cloneLabels(postLabels)
	} else {
		exec.CompletionLabels = cloneLabels(taskCtx.Issue.Labels)
	}

	if result == nil {
		result = infraFailureResult("session.Run returned nil result", runErr)
		exec.FailureSource = "session_run_nil_result"
	}
	if runErr != nil && result.Stderr == "" {
		result.Stderr = runErr.Error()
	}
	if exec.FailureSource == "" && runtimepkg.IsInfraFailure(result) {
		exec.FailureSource = "launcher_infra_failure"
	}

	exec.CompletedAt = time.Now().UTC()
	exec.Result = result
	exec.RunErr = runErr
	sessionStatus = executionStatus(result, runErr)
	return exec
}

func (e *Executor) withLifecycleContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}

	e.mu.Lock()
	lifecycle := e.lifecycle
	e.mu.Unlock()
	if lifecycle == nil {
		return parent, func() {}
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-lifecycle.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

func infraFailureResult(reason string, err error) *runtimepkg.Result {
	stderr := ""
	if err != nil {
		stderr = err.Error()
	}
	return &runtimepkg.Result{
		ExitCode: -1,
		Stderr:   stderr,
		Meta: map[string]string{
			runtimepkg.MetaInfraFailure:       "true",
			runtimepkg.MetaInfraFailureReason: reason,
		},
	}
}

func executionStatus(result *runtimepkg.Result, runErr error) string {
	if result != nil && result.Meta != nil && result.Meta["timeout"] == "true" {
		return store.TaskStatusTimeout
	}
	if runErr != nil || (result != nil && result.ExitCode != 0) {
		return store.TaskStatusFailed
	}
	return store.TaskStatusCompleted
}

func closeManagedSession(taskCtx *runtimepkg.TaskContext, status string) error {
	if taskCtx == nil || taskCtx.SessionHandle() == nil {
		return nil
	}
	if status == "" {
		status = store.TaskStatusCompleted
	}
	return taskCtx.SessionHandle().Close(status)
}

// StreamSessionEvents exposes the canonical event-stream artifact writer for tests
// and callers that need the shared session layout behavior directly.
func StreamSessionEvents(taskCtx *runtimepkg.TaskContext, eventsCh <-chan launcherevents.Event) (string, func() error) {
	return workersession.Stream(taskCtx, eventsCh)
}

func snapshotIssueLabels(repo string, issueNum int, reader IssueLabelReader) ([]string, error) {
	if reader == nil {
		return nil, fmt.Errorf("no issue label reader configured")
	}
	labels, err := reader.ReadIssueLabels(repo, issueNum)
	if err != nil {
		return nil, err
	}
	return cloneLabels(labels), nil
}

func cloneLabels(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	return append([]string(nil), labels...)
}

type issueTaskLocks struct {
	mu    sync.Mutex
	locks map[string]*issueTaskLock
}

type issueTaskLock struct {
	mu     sync.Mutex
	parent *issueTaskLocks
	key    string
	refs   int
}

func (l *issueTaskLocks) Acquire(repo string, issue int) *issueTaskLock {
	key := fmt.Sprintf("%s#%d", repo, issue)

	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*issueTaskLock)
	}
	lk, ok := l.locks[key]
	if !ok {
		lk = &issueTaskLock{parent: l, key: key}
		l.locks[key] = lk
	}
	lk.refs++
	l.mu.Unlock()

	lk.mu.Lock()
	return lk
}

func (l *issueTaskLock) Release() {
	l.mu.Unlock()

	l.parent.mu.Lock()
	l.refs--
	if l.refs == 0 {
		if current, ok := l.parent.locks[l.key]; ok && current == l {
			delete(l.parent.locks, l.key)
		}
	}
	l.parent.mu.Unlock()
}
