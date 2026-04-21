package cmd

import (
	"runtime"
	"time"

	"github.com/Lincyaw/workbuddy/internal/router"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
)

type issueLabelReader interface {
	ReadIssueLabels(repo string, issueNum int) ([]string, error)
}

func defaultEmbeddedWorkerParallelism() int {
	if runtime.NumCPU() < defaultMaxParallelTasks {
		return runtime.NumCPU()
	}
	return defaultMaxParallelTasks
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
