package cmd

// This file used to own the live-reload plumbing (notifierRuntime +
// coordinatorConfigRuntime + fsnotify watcher). That body moved to
// internal/app/config_reload.go as part of #145-7. The aliases below keep
// the cmd-side call sites working.

import (
	"github.com/Lincyaw/workbuddy/internal/app"
)

type (
	notifierRuntime          = app.NotifierRuntime
	coordinatorConfigRuntime = app.CoordinatorConfigRuntime
	configReloadSummary      = app.ConfigReloadSummary
)

var (
	newNotifierRuntime             = app.NewNotifierRuntime
	newCoordinatorConfigRuntime    = app.NewCoordinatorConfigRuntime
	startCoordinatorConfigWatcher  = app.StartCoordinatorConfigWatcher
	validateNotificationsConfig    = app.ValidateNotificationsConfig
)
