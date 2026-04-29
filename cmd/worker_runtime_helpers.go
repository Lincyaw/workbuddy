package cmd

import (
	"github.com/Lincyaw/workbuddy/internal/app"
)

// issueLabelReader is the cmd-side alias for app.IssueLabelReader so existing
// call sites (worker_test, worker, serve) keep compiling after the app split.
type issueLabelReader = app.IssueLabelReader

type (
	RunningTasks = app.RunningTasks
	closedIssues = app.ClosedIssues
	GHCLIReader  = app.GHCLIReader
)

var (
	publishTaskCompletion = app.PublishTaskCompletion
	NewRunningTasks       = app.NewRunningTasks
	recoverTasks          = app.RecoverTasks
	allowSecurityEvent    = app.AllowSecurityEvent
	logSecurityPosture    = app.LogSecurityPosture
	newTaskWatchHandler   = app.NewTaskWatchHandler
)
