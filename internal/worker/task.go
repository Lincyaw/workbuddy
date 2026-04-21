package worker

import (
	"context"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

type SessionRunner func(ctx context.Context, session runtimepkg.Session, eventsCh chan<- launcherevents.Event) (*runtimepkg.Result, error)

// Task is the canonical execution input shared by worker implementations.
type Task struct {
	TaskID            string
	Repo              string
	IssueNum          int
	AgentName         string
	Agent             *config.AgentConfig
	Context           *runtimepkg.TaskContext
	Workflow          string
	State             string
	WorkerID          string
	Attempt           int
	Cleanup           func() error
	RunSession        SessionRunner
	EventDrainTimeout time.Duration
}

// Execution captures the shared runtime/session execution outcome.
type Execution struct {
	Task             Task
	Result           *runtimepkg.Result
	RunErr           error
	StartedAt        time.Time
	CompletedAt      time.Time
	PreLabels        []string
	PostLabels       []string
	CompletionLabels []string
	PreSnapshotErr   error
	PostSnapshotErr  error
	EventErr         error
	FailureSource    string
}

func (e Execution) ExitCode() int {
	if e.Result == nil {
		return -1
	}
	return e.Result.ExitCode
}

func (e Execution) Status() string {
	if e.Result != nil && e.Result.Meta != nil && e.Result.Meta["timeout"] == "true" {
		return "timeout"
	}
	if e.RunErr != nil || e.ExitCode() != 0 {
		return "failed"
	}
	return "completed"
}

func (e Execution) InfraFailure() bool {
	return runtimepkg.IsInfraFailure(e.Result)
}

func (e Execution) InfraReason() string {
	if e.Result == nil || e.Result.Meta == nil {
		return ""
	}
	return e.Result.Meta[runtimepkg.MetaInfraFailureReason]
}
