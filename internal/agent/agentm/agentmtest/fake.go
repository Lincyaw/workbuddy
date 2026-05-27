// Package agentmtest provides a fake AgentM binary builder for unit tests.
// It mirrors the codextest pattern (sibling fake harness for the codex
// app-server) but for the real AgentM CLI shape:
//
//	agentm "<prompt>" --scenario <name> [-e module[:json] ...] \
//	    [--cwd <workspace>] [--max-turns N] [--model M]
//
// AgentM streams the agent's text to stdout and prints a `====` summary
// banner; the agent's final human-readable text is the last assistant
// paragraph before that banner. There is no RESULT: line in normal CLI mode.
// The coordinator-managed modes (failure / malformed / missing-required)
// still emit a RESULT: line so the legacy GitOps/LabelWriter path stays
// exercised.
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
	// ModeSuccess streams an agent paragraph + the `====` summary banner and
	// exits 0 with NO RESULT: line. This is the normal CLI-mode success: the
	// backend MUST capture the trailing paragraph as FinalMsg and treat the
	// run as success.
	ModeSuccess Mode = "success"
	// ModeFailure emits a well-formed RESULT: line with success=false plus
	// a failure_reason and exits 0 (coordinator-managed path). The bridge
	// MUST surface the failure_reason and apply the agent's next_label.
	ModeFailure Mode = "failure"
	// ModeResultSuccess emits a well-formed RESULT: line with success=true
	// (coordinator-managed path). Asserts the structured Output/next_label
	// path still works alongside the new CLI-text path.
	ModeResultSuccess Mode = "result-success"
	// ModeMalformedJSON emits a RESULT: line whose body is not valid JSON.
	// This MUST be classified as an infra failure.
	ModeMalformedJSON Mode = "malformed-json"
	// ModeNoResult emits stdout transcript and exits 0 with no RESULT: line
	// and no `====` summary banner. In CLI mode this is SUCCESS whose
	// FinalMsg is the trailing stdout text.
	ModeNoResult Mode = "no-result"
	// ModeMissingRequired emits a RESULT: line that is missing a required
	// field per the output schema (no next_label). MUST surface a schema
	// violation as an infra failure.
	ModeMissingRequired Mode = "missing-required"
)

// Config configures the fake binary.
type Config struct {
	Mode Mode
	// NextLabel overrides the default "status:review" in ModeResultSuccess /
	// "status:failed" in ModeFailure.
	NextLabel string
	// FailureReason overrides the default reason in ModeFailure.
	FailureReason string
	// FinalText overrides the agent paragraph emitted in ModeSuccess /
	// ModeNoResult. Defaults to a recognizable sentinel.
	FinalText string
	// EnvDumpPath, when non-empty, makes the fake write its full process
	// environment (one `KEY=VALUE` per line) to this absolute path before
	// emitting output. Tests use it to assert workbuddy-injected env vars
	// (TRACEPARENT, AGENTM_AGENT_ENV_IMAGE, …) actually reach the
	// AgentM subprocess.
	EnvDumpPath string
	// ArgvDumpPath, when non-empty, makes the fake write its argv (one arg
	// per line) to this absolute path. Tests assert the new CLI invocation
	// form (positional prompt, --scenario, -e pairs, --cwd, --max-turns).
	ArgvDumpPath string
}

// DefaultFinalText is the agent paragraph the success/no-result fakes emit
// when Config.FinalText is empty.
const DefaultFinalText = "All acceptance criteria are met; the change is complete."

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
	finalText := cfg.FinalText
	if finalText == "" {
		finalText = DefaultFinalText
	}

	// emitResult holds a bash assignment of BODY for the RESULT: modes; empty
	// for the CLI-text modes.
	var emitResult string
	switch mode {
	case ModeSuccess, ModeNoResult:
		emitResult = ""
	case ModeResultSuccess:
		if nextLabel == "" {
			nextLabel = "status:review"
		}
		// session_log_path is required by the schema when success=true; point
		// it at a file the script writes below so validation passes.
		emitResult = fmt.Sprintf(`BODY='{"success":true,"next_label":"%s","session_log_path":"'"$RESULT_SESSION_LOG"'"}'`, nextLabel)
	case ModeFailure:
		if nextLabel == "" {
			nextLabel = "status:failed"
		}
		if failureReason == "" {
			failureReason = "fake agentm reports failure"
		}
		emitResult = fmt.Sprintf(`BODY='{"success":false,"next_label":"%s","failure_reason":"%s"}'`,
			nextLabel, failureReason)
	case ModeMalformedJSON:
		emitResult = `BODY='{not valid json at all'`
	case ModeMissingRequired:
		emitResult = `BODY='{"success":true}'`
	default:
		t.Fatalf("agentmtest: unknown mode %q", mode)
	}

	// The fake dumps argv/env first (for assertions), then streams an agent
	// transcript. The first positional arg is the prompt under the new CLI.
	script := "#!/usr/bin/env bash\nset -eu\n"
	if cfg.ArgvDumpPath != "" {
		script += fmt.Sprintf("printf '%%s\\n' \"$@\" > %q\n", cfg.ArgvDumpPath)
	}
	if cfg.EnvDumpPath != "" {
		script += fmt.Sprintf("env > %q\n", cfg.EnvDumpPath)
	}

	// Stream a short transcript: a tool call line (decoration) then the
	// agent's final paragraph, mirroring AgentM's streaming presenter.
	script += "echo '→ read_file(path=README.md)'\n"
	script += fmt.Sprintf("echo %q\n", finalText)
	script += "echo\n"

	switch mode {
	case ModeSuccess, ModeResultSuccess, ModeFailure, ModeMalformedJSON, ModeMissingRequired:
		// Emit the AgentM `====` summary banner so final-text accumulation
		// stops before the run accounting lines.
		script += "echo '============================================================'\n"
		script += "echo 'messages=2 tool_calls=1'\n"
		script += "echo 'tokens: in=10 out=5 cache_r=0 cache_w=0 (over 1 turn)'\n"
	case ModeNoResult:
		// No summary banner: FinalMsg comes from the trailing paragraph.
	}

	if emitResult != "" {
		// Provide a real session-log path for the RESULT: session_log_path
		// field (schema-required when success=true).
		script += fmt.Sprintf("RESULT_SESSION_LOG=%q\n", filepath.Join(dir, "result-session.jsonl"))
		script += `printf '{"kind":"turn.completed"}\n' > "$RESULT_SESSION_LOG"` + "\n"
		script += emitResult + "\n"
		script += `echo "RESULT: $BODY"` + "\n"
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
