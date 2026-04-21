package cmd

// This file used to own the multi-repo runtime (pollerManager + RepoRuntime).
// That body moved to internal/app/repo_runtime.go as part of #145-7: the CLI
// layer no longer owns runtime-assembly. The aliases below keep the existing
// cmd-side call sites (and tests) working without a rewrite.

import (
	"github.com/Lincyaw/workbuddy/internal/app"
)

type (
	repoRegistrationPayload = app.RepoRegistrationPayload
	repoRuntime             = app.RepoRuntime
	repoStatus              = app.RepoStatus
	pollerManager           = app.PollerManager
)

var (
	newPollerManager             = app.NewPollerManager
	buildRepoRegistrationPayload = app.BuildRepoRegistrationPayload
	buildRepoRegistrationRecord  = app.BuildRepoRegistrationRecord
	decodeRepoRegistrationConfig = app.DecodeRepoRegistrationConfig
)
