package launcher

import runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"

const (
	MetaInfraFailure       = runtimepkg.MetaInfraFailure
	MetaInfraFailureReason = runtimepkg.MetaInfraFailureReason
)

func isExecStartError(err error) bool {
	return runtimepkg.IsExecStartError(err)
}

func isScannerBufferOverflow(err error) bool {
	return runtimepkg.IsScannerBufferOverflow(err)
}

func stderrLooksLikeRuntimePanic(stderr string) bool {
	return runtimepkg.StderrLooksLikeRuntimePanic(stderr)
}

func markInfraFailure(result *Result, reason string) {
	runtimepkg.MarkInfraFailure(result, reason)
}

func IsInfraFailure(result *Result) bool {
	return runtimepkg.IsInfraFailure(result)
}
