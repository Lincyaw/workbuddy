// Package launcher distinguishes launcher-layer infrastructure failures
// (exec errors, scanner buffer overflows, runtime panics before any LLM
// output) from genuine agent verdicts so downstream reporting and state
// management can treat them separately.
//
// An "infra failure" means the agent process was not able to deliver a
// verdict the coordinator can trust. Examples:
//   - exec.Error from cmd.Start() (binary missing, PATH misconfigured)
//   - a non-ExitError wrapping from cmd.Wait() (kernel-level issue, IO)
//   - bufio.Scanner hitting bufio.ErrTooLong (unbounded JSONL line)
//   - a Rust plugin-cache panic or similar runtime abort before the agent
//     emitted any tool/message events (e.g., missing env var, cache corrupt)
//
// Callers must NOT treat an infra failure as a FAIL verdict for the
// state-machine. It is not the agent disagreeing with itself; it is the
// launcher being unable to run the agent at all.
package launcher

import (
	"bufio"
	"errors"
	"os/exec"
	"strings"
)

// MetaInfraFailure is the Result.Meta key that marks an infrastructure-level
// failure. Reporter and worker paths read this to render "Infra Error" and
// suppress state-machine failure induction.
const MetaInfraFailure = "infra_failure"

// MetaInfraFailureReason carries a short, operator-facing reason string
// describing why the launcher considered this run an infra failure.
const MetaInfraFailureReason = "infra_failure_reason"

// Recognizable stderr patterns for runtime panics / aborts that occur before
// the agent has a chance to emit any output. These indicate the launcher
// never reached an "agent decided" state.
var infraFailureStderrPatterns = []string{
	// Rust panic signature (codex ships a Rust binary; e.g., plugin-cache).
	"panicked at",
	// Common codex plugin-cache bail messages observed in production.
	"plugin-cache",
	// Go runtime signatures when the runtime harness (not the agent) aborts.
	"runtime error:",
	"fatal error:",
}

// isExecStartError returns true when err is an exec.Error — i.e., the
// process could not be started at all (binary not on PATH, permission
// denied, etc.). This is strictly pre-LLM and always an infra failure.
func isExecStartError(err error) bool {
	if err == nil {
		return false
	}
	var execErr *exec.Error
	return errors.As(err, &execErr)
}

// isScannerBufferOverflow returns true when err wraps bufio.ErrTooLong,
// which means the runtime produced a single JSONL line longer than the
// scanner buffer. We cannot trust partial output, so we classify this as
// an infra failure rather than attribute a verdict to the agent.
func isScannerBufferOverflow(err error) bool {
	return err != nil && errors.Is(err, bufio.ErrTooLong)
}

// stderrLooksLikeRuntimePanic returns true when stderr contains a recognizable
// launcher-layer panic/abort signature (Rust panic, Go runtime fatal, etc.).
// Callers should only apply this check when they can also confirm that no
// agent output was observed, to avoid mis-classifying an agent's own tool
// output that happens to mention the word "panic".
func stderrLooksLikeRuntimePanic(stderr string) bool {
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

// markInfraFailure sets Result.Meta[MetaInfraFailure] = "true" along with a
// short reason, creating the Meta map if needed. Safe to call on a nil
// Result (no-op).
func markInfraFailure(result *Result, reason string) {
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

// IsInfraFailure returns true when the given Result was produced by a
// launcher-layer infrastructure failure rather than an agent verdict.
func IsInfraFailure(result *Result) bool {
	if result == nil || result.Meta == nil {
		return false
	}
	return result.Meta[MetaInfraFailure] == "true"
}
