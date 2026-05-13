// Package agentmtest provides a fake AgentM binary builder for unit tests.
// It mirrors the codextest pattern (sibling fake harness for the codex
// app-server) but for AgentM's stdout-RESULT contract.
//
// Usage:
//
//	bin := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeSuccess})
//	be := &agentm.Backend{Binary: bin}
//	sess, _ := be.NewSession(ctx, agent.Spec{...})
//	res, _ := sess.Wait(ctx)
package agentmtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Mode picks the fake's behaviour. The fake is a shell script so it runs on
// any host workbuddy itself builds on (no compile step in the inner test loop).
type Mode string

const (
	// ModeSuccess emits a well-formed RESULT: line with success=true plus a
	// dummy session log path, then exits 0.
	ModeSuccess Mode = "success"
	// ModeFailure emits a well-formed RESULT: line with success=false plus
	// a failure_reason and exits 0. The bridge MUST still surface the
	// failure_reason and apply the agent's next_label.
	ModeFailure Mode = "failure"
	// ModeMalformedJSON emits a RESULT: line whose body is not valid JSON
	// (and no result file). This MUST be classified as an infra failure.
	ModeMalformedJSON Mode = "malformed-json"
	// ModeNoResult emits stdout/stderr transcript but no RESULT: line.
	// MUST be classified as an infra failure.
	ModeNoResult Mode = "no-result"
	// ModeMissingRequired emits a JSON object that is missing a required
	// field per the output schema (no next_label). MUST surface a schema
	// violation as the failure_reason.
	ModeMissingRequired Mode = "missing-required"
)

// Config configures the fake binary.
type Config struct {
	Mode Mode
	// NextLabel overrides the default "status:review" in ModeSuccess /
	// "status:failed" in ModeFailure.
	NextLabel string
	// FailureReason overrides the default reason in ModeFailure.
	FailureReason string
	// EnvDumpPath, when non-empty, makes the fake write its full process
	// environment (one `KEY=VALUE` per line) to this absolute path before
	// emitting RESULT:. Tests use it to assert workbuddy-injected env vars
	// (TRACEPARENT, AGENTM_AGENT_ENV_IMAGE, …) actually reach the
	// AgentM subprocess.
	EnvDumpPath string
}

// BuildFake writes a shell-script fake to a temp file marked executable and
// returns its absolute path. The script is removed via t.Cleanup.
func BuildFake(t *testing.T, cfg Config) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("agentmtest: fake binary is a POSIX shell script; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agentm")

	mode := cfg.Mode
	if mode == "" {
		mode = ModeSuccess
	}
	nextLabel := cfg.NextLabel
	failureReason := cfg.FailureReason

	// emitResult is a bash snippet that writes the RESULT: line to stdout
	// AND the result file. Variables $SESSION_LOG / $RESULT_FILE are bash
	// expansions; everything else must be safe inside single-quotes.
	var emitResult string
	switch mode {
	case ModeSuccess:
		if nextLabel == "" {
			nextLabel = "status:review"
		}
		emitResult = fmt.Sprintf(`BODY='{"success":true,"next_label":"%s","session_log_path":"'"$SESSION_LOG"'"}'`, nextLabel)
	case ModeFailure:
		if nextLabel == "" {
			nextLabel = "status:failed"
		}
		if failureReason == "" {
			failureReason = "fake agentm reports failure"
		}
		emitResult = fmt.Sprintf(`BODY='{"success":false,"next_label":"%s","failure_reason":"%s","session_log_path":"'"$SESSION_LOG"'"}'`,
			nextLabel, failureReason)
	case ModeMalformedJSON:
		emitResult = `BODY='{not valid json at all'`
	case ModeNoResult:
		emitResult = ""
	case ModeMissingRequired:
		emitResult = `BODY='{"success":true,"session_log_path":"'"$SESSION_LOG"'"}'`
	default:
		t.Fatalf("agentmtest: unknown mode %q", mode)
	}

	// Parse --result-file / --session-log / --task-file out of $@. The fake
	// always touches the session log so the host can ingest a non-empty
	// artifact even when the contract is malformed.
	script := `#!/usr/bin/env bash
set -eu
WORKSPACE=""
TASK_FILE=""
SESSION_LOG=""
RESULT_FILE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --workspace) WORKSPACE="$2"; shift 2 ;;
    --task-file) TASK_FILE="$2"; shift 2 ;;
    --session-log) SESSION_LOG="$2"; shift 2 ;;
    --result-file) RESULT_FILE="$2"; shift 2 ;;
    *) shift ;;
  esac
done

if [[ -n "$SESSION_LOG" ]]; then
  mkdir -p "$(dirname "$SESSION_LOG")"
  cat > "$SESSION_LOG" <<'JSONL'
{"kind":"turn.started","ts":"2026-05-13T00:00:00Z"}
{"kind":"agent.message","ts":"2026-05-13T00:00:01Z","text":"fake agentm running"}
{"kind":"turn.completed","ts":"2026-05-13T00:00:02Z"}
JSONL
fi

echo "fake agentm starting in $WORKSPACE"
echo "task file: $TASK_FILE"
`
	if cfg.EnvDumpPath != "" {
		script += fmt.Sprintf("env > %q\n", cfg.EnvDumpPath)
	}
	if emitResult != "" {
		script += emitResult + "\n"
		script += `echo "RESULT: $BODY"` + "\n"
		script += `if [[ -n "$RESULT_FILE" ]]; then echo "$BODY" > "$RESULT_FILE"; fi` + "\n"
	}
	script += "exit 0\n"

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("agentmtest: write fake: %v", err)
	}
	// Quick sanity: ensure bash is present.
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("agentmtest: bash not on PATH: %v", err)
	}
	return path
}
