package launcher

import (
	"bufio"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// TestProcessRuntime_RuntimePanicSignatureMarksInfraFailure exercises
// the non-stream process session (claude-code without a prompt, falling
// through to the plain sh -c wrapper). When the wrapped command exits
// non-zero with no stdout and a stderr that matches a runtime-panic
// signature, the process runtime must mark Meta[infra_failure] so the
// reporter renders "Infra Error" instead of "Failure".
func TestProcessRuntime_RuntimePanicSignatureMarksInfraFailure(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	// The command emits a Rust-style panic to stderr and exits 101 with
	// no stdout. This is exactly the plugin-cache scenario the reporter
	// was previously mis-classifying as an agent FAIL verdict.
	agent := &config.AgentConfig{
		Name:    "panic-sim",
		Runtime: config.RuntimeClaudeCode,
		Command: `printf "thread 'main' panicked at src/plugin-cache.rs: boom\n" >&2; exit 101`,
		Timeout: 5 * time.Second,
	}
	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("Launch unexpectedly errored: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !IsInfraFailure(result) {
		t.Fatalf("expected Meta[infra_failure]=true for runtime panic, got %v (stderr=%q stdout=%q)", result.Meta, result.Stderr, result.Stdout)
	}
}

// TestProcessRuntime_GenuineExitNoInfraFailure verifies backwards
// compatibility: a plain non-zero exit with agent-flavored stderr must
// NOT be marked infra_failure and should render as a regular "Failure"
// downstream. Without this guard, we would over-report infra errors and
// lose the original FAIL verdict semantics. See AC-5.
func TestProcessRuntime_GenuineExitNoInfraFailure(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "fail-sim",
		Runtime: config.RuntimeClaudeCode,
		Command: `echo "agent says: unit tests failed"; exit 2`,
		Timeout: 5 * time.Second,
	}
	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if IsInfraFailure(result) {
		t.Fatalf("genuine FAIL verdict must NOT be marked infra_failure, got Meta=%v", result.Meta)
	}
	if result.ExitCode != 2 {
		t.Fatalf("expected exit code 2, got %d", result.ExitCode)
	}
}

// TestClaudeStream_InfraFailureMetaHelpers asserts the shared
// markInfraFailure helper invariants used by the runClaudeStream "read
// stdout" / "build command" / "cmd.Start" error paths.
func TestClaudeStream_InfraFailureMetaHelpers(t *testing.T) {
	r := &Result{ExitCode: -1, Meta: map[string]string{}}
	markInfraFailure(r, "stdout stream read error")
	if !IsInfraFailure(r) {
		t.Fatalf("expected infra-failure marker, got %v", r.Meta)
	}
	if r.Meta[MetaInfraFailureReason] != "stdout stream read error" {
		t.Fatalf("unexpected reason: %q", r.Meta[MetaInfraFailureReason])
	}
}

// TestStderrLooksLikeRuntimePanic covers the detection heuristic in
// isolation so regressions to the pattern list are caught.
func TestStderrLooksLikeRuntimePanic(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"empty", "", false},
		{"agent complaint", "agent: failing this PR because tests broke", false},
		{"rust panic", "thread 'main' panicked at src/lib.rs:10:5: boom", true},
		{"plugin-cache bail", "plugin-cache: integrity check failed", true},
		{"go runtime error", "runtime error: invalid memory address", true},
		{"go fatal", "fatal error: concurrent map writes", true},
		{"case insensitivity", "Thread Main PANICKED AT somewhere", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stderrLooksLikeRuntimePanic(tc.stderr); got != tc.want {
				t.Fatalf("stderrLooksLikeRuntimePanic(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

// TestIsScannerBufferOverflow covers the scanner-overflow detection
// helper used by the codex stream path.
func TestIsScannerBufferOverflow(t *testing.T) {
	if !isScannerBufferOverflow(bufio.ErrTooLong) {
		t.Fatal("expected bufio.ErrTooLong to be recognized")
	}
	if !isScannerBufferOverflow(fmt.Errorf("wrapped: %w", bufio.ErrTooLong)) {
		t.Fatal("expected wrapped bufio.ErrTooLong to be recognized")
	}
	if isScannerBufferOverflow(fmt.Errorf("some other error")) {
		t.Fatal("expected non-ErrTooLong error not to be recognized")
	}
	if isScannerBufferOverflow(nil) {
		t.Fatal("expected nil error not to be recognized")
	}
}

// TestMarkInfraFailure covers the Meta-mutation helper edge cases.
func TestMarkInfraFailure(t *testing.T) {
	// nil result is a no-op, not a panic.
	markInfraFailure(nil, "anything")

	// Result with nil Meta gets a new map.
	r := &Result{}
	markInfraFailure(r, "reason-a")
	if r.Meta == nil || r.Meta[MetaInfraFailure] != "true" {
		t.Fatalf("expected infra_failure=true in freshly-created Meta, got %v", r.Meta)
	}
	if r.Meta[MetaInfraFailureReason] != "reason-a" {
		t.Fatalf("expected reason-a, got %q", r.Meta[MetaInfraFailureReason])
	}

	// Existing Meta is preserved.
	r2 := &Result{Meta: map[string]string{"timeout": "true"}}
	markInfraFailure(r2, "reason-b")
	if r2.Meta["timeout"] != "true" {
		t.Fatal("expected pre-existing timeout=true to survive")
	}
	if r2.Meta[MetaInfraFailure] != "true" {
		t.Fatal("expected infra_failure=true")
	}
}

