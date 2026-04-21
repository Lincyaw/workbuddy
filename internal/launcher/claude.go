// Package launcher provides multi-runtime agent execution backends.
package launcher

import runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"

type ClaudeRuntime = runtimepkg.ClaudeRuntime

func findClaudeSessionPath(repoPath string) string {
	return runtimepkg.FindClaudeSessionPath(repoPath)
}
