package launcher

import runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"

func parseMeta(stdout string) map[string]string {
	return runtimepkg.ParseMeta(stdout)
}
