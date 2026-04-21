package runtime

import (
	"bufio"
	"errors"
	"os/exec"
	"strings"
)

const (
	MetaInfraFailure       = "infra_failure"
	MetaInfraFailureReason = "infra_failure_reason"
	MetaStorageDegraded    = "storage_degraded"
	MetaStorageIssues      = "storage_issues"
)

var infraFailureStderrPatterns = []string{
	"panicked at",
	"plugin-cache",
	"runtime error:",
	"fatal error:",
}

func IsExecStartError(err error) bool {
	if err == nil {
		return false
	}
	var execErr *exec.Error
	return errors.As(err, &execErr)
}

func IsScannerBufferOverflow(err error) bool {
	return err != nil && errors.Is(err, bufio.ErrTooLong)
}

func StderrLooksLikeRuntimePanic(stderr string) bool {
	if stderr == "" {
		return false
	}
	low := strings.ToLower(stderr)
	for _, needle := range infraFailureStderrPatterns {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

func MarkInfraFailure(result *Result, reason string) {
	if result == nil {
		return
	}
	if result.Meta == nil {
		result.Meta = map[string]string{}
	}
	result.Meta[MetaInfraFailure] = "true"
	if reason != "" {
		result.Meta[MetaInfraFailureReason] = reason
	}
}

func IsInfraFailure(result *Result) bool {
	if result == nil || result.Meta == nil {
		return false
	}
	return result.Meta[MetaInfraFailure] == "true"
}
