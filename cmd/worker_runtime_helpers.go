package cmd

import (
	"github.com/Lincyaw/workbuddy/internal/app"
)

// issueLabelReader is the cmd-side alias for app.IssueLabelReader so existing
// call sites (worker_test, worker, serve) keep compiling after the app split.
type issueLabelReader = app.IssueLabelReader

var (
	defaultEmbeddedWorkerParallelism = app.DefaultEmbeddedWorkerParallelism
	publishTaskCompletion            = app.PublishTaskCompletion
)
