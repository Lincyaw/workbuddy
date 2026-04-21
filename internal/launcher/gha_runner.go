package launcher

import (
	"github.com/Lincyaw/workbuddy/internal/config"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

type ghaSession = runtimepkg.GHASession

func newGHASession(agent *config.AgentConfig, task *TaskContext) Session {
	return runtimepkg.NewGHASession(agent, task)
}

func findDownloadedPath(files []string, reported string) string {
	return runtimepkg.FindDownloadedPath(files, reported)
}

func sessionArtifactDir(task *TaskContext) string {
	return runtimepkg.SessionArtifactDir(task)
}
