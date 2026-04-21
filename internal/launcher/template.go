package launcher

import runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"

func shellEscape(s string) string {
	return runtimepkg.ShellEscape(s)
}

func renderCommand(cmdTemplate string, task *TaskContext) (string, error) {
	return runtimepkg.RenderCommand(cmdTemplate, task)
}

func renderCommandRaw(cmdTemplate string, task *TaskContext) (string, error) {
	return runtimepkg.RenderCommandRaw(cmdTemplate, task)
}

func extractPrompt(rendered string) (prompt string, extraArgs []string, ok bool) {
	return runtimepkg.ExtractPrompt(rendered)
}
