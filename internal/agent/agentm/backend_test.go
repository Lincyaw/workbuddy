package agentm_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm/agentmtest"
)

func newSpec(t *testing.T) agent.Spec {
	t.Helper()
	work := t.TempDir()
	return agent.Spec{
		Backend:  "agentm",
		Workdir:  work,
		Prompt:   "do the thing",
		Scenario: "agent_env",
		Env: map[string]string{
			"WORKBUDDY_ISSUE_NUMBER": "319",
			"WORKBUDDY_REPO":         "Lincyaw/workbuddy",
			"WORKBUDDY_SESSION_ID":   "test-session",
			"TRACEPARENT":            "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		},
	}
}

func waitForSession(ctx context.Context, t *testing.T, sess agent.Session) agent.Result {
	t.Helper()
	doneEvt := make(chan struct{})
	go func() {
		//nolint:revive // draining the event channel until close
		for range sess.Events() {
		}
		close(doneEvt)
	}()
	res, err := sess.Wait(ctx)
	<-doneEvt
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	return res
}

// TestBackend_CLISuccess_CapturesFinalText is the new normal mode: AgentM
// exits 0 with no RESULT: line; the backend treats it as success and surfaces
// the trailing assistant paragraph as FinalMsg.
func TestBackend_CLISuccess_CapturesFinalText(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:      agentmtest.ModeSuccess,
		FinalText: "Implemented REQ-134 and all tests pass.",
	})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	res := waitForSession(ctx, t, sess)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
	if res.FinalMsg != "Implemented REQ-134 and all tests pass." {
		t.Fatalf("FinalMsg = %q, want the trailing assistant paragraph", res.FinalMsg)
	}

	// No RESULT: line means Output() returns (nil, nil) — not an error.
	extractor := sess.(interface {
		Output() (*agentm.Output, error)
		SessionLogPath() string
	})
	out, perr := extractor.Output()
	if perr != nil {
		t.Fatalf("Output() should not error in CLI mode: %v", perr)
	}
	if out != nil {
		t.Fatalf("Output() should be nil with no RESULT: line, got %+v", out)
	}

	// The captured stdout transcript is the session-log artifact.
	logPath := extractor.SessionLogPath()
	if logPath == "" {
		t.Fatalf("expected session log path")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("session log missing: %v", err)
	}
	if !strings.Contains(string(data), "Implemented REQ-134") {
		t.Fatalf("session log missing transcript text:\n%s", data)
	}
}

// TestBackend_NoResultNoBanner_IsSuccess covers exit-0 with neither a RESULT:
// line nor a `====` summary banner — the trailing stdout text is FinalMsg.
func TestBackend_NoResultNoBanner_IsSuccess(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:      agentmtest.ModeNoResult,
		FinalText: "Done — nothing else to do.",
	})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	res := waitForSession(ctx, t, sess)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
	if res.FinalMsg != "Done — nothing else to do." {
		t.Fatalf("FinalMsg = %q", res.FinalMsg)
	}
}

// TestBackend_BuildArgs_NewCLIShape asserts the backend emits the real AgentM
// CLI argv: positional prompt first, then --scenario, -e pairs, --cwd, and
// --max-turns. No more run/--task-file/--result-file/--session-log.
func TestBackend_BuildArgs_NewCLIShape(t *testing.T) {
	argvDump := filepath.Join(t.TempDir(), "argv")
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:         agentmtest.ModeSuccess,
		ArgvDumpPath: argvDump,
	})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	work := t.TempDir()
	spec := agent.Spec{
		Backend:  "agentm",
		Workdir:  work,
		Prompt:   "resolve issue #319",
		Scenario: "agent_env",
		Model:    "claude-sonnet",
		MaxTurns: 40,
		Extensions: []agent.SpecExtension{
			{Module: "agentm.extensions.builtin.system_prompt", Config: map[string]any{"prompt": "be terse"}},
			{Module: "llmharness.adapters.agentm"},
		},
	}

	sess, err := be.NewSession(ctx, spec)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	waitForSession(ctx, t, sess)

	data, err := os.ReadFile(argvDump)
	if err != nil {
		t.Fatalf("read argv dump: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(args) == 0 || args[0] != "resolve issue #319" {
		t.Fatalf("argv[0] must be the positional prompt, got %v", args)
	}
	joined := strings.Join(args, "\x00")
	mustContainSeq(t, args, "--scenario", "agent_env")
	mustContainSeq(t, args, "--model", "claude-sonnet")
	mustContainSeq(t, args, "--cwd", work)
	mustContainSeq(t, args, "--max-turns", "40")
	mustContainSeq(t, args, "-e", `agentm.extensions.builtin.system_prompt:{"prompt":"be terse"}`)
	mustContainSeq(t, args, "-e", "llmharness.adapters.agentm")
	for _, banned := range []string{"run", "--task-file", "--result-file", "--session-log", "--workspace"} {
		if strings.Contains(joined, "\x00"+banned+"\x00") || args[0] == banned {
			t.Fatalf("argv must not contain legacy flag %q: %v", banned, args)
		}
	}
}

func mustContainSeq(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Fatalf("argv missing %q %q in %v", flag, value, args)
}

func TestBackend_Close_RemovesTempDir(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeSuccess})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	waitForSession(ctx, t, sess)

	extractor := sess.(interface {
		SessionLogPath() string
	})
	logPath := extractor.SessionLogPath()
	if logPath == "" {
		t.Fatalf("expected session log path")
	}
	tmpDir := filepath.Dir(logPath)
	if _, err := os.Stat(tmpDir); err != nil {
		t.Fatalf("temp dir missing before close: %v", err)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Fatalf("temp dir %q still exists after Close (err=%v): agentm dispatch leaks /tmp", tmpDir, err)
	}

	// Close must be idempotent.
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestBackend_ResultSuccess_PreservesStructuredPath asserts the legacy
// coordinator-managed RESULT: line path still parses into a structured
// Output (success + next_label) when AgentM emits one.
func TestBackend_ResultSuccess_PreservesStructuredPath(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeResultSuccess})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	waitForSession(ctx, t, sess)

	extractor := sess.(interface {
		Output() (*agentm.Output, error)
		SessionLogPath() string
	})
	out, perr := extractor.Output()
	if perr != nil {
		t.Fatalf("Output(): %v", perr)
	}
	if out == nil || !out.Success {
		t.Fatalf("expected structured success Output, got %+v", out)
	}
	if out.NextLabel != "status:review" {
		t.Fatalf("expected next_label=status:review, got %q", out.NextLabel)
	}
}

func TestBackend_MalformedJSON_IsInfraFailure(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeMalformedJSON})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	go func() {
		for range sess.Events() {
		}
	}()
	_, err = sess.Wait(ctx)
	if err == nil {
		t.Fatalf("expected wait error for malformed RESULT")
	}
	extractor := sess.(interface {
		Output() (*agentm.Output, error)
	})
	out, perr := extractor.Output()
	if perr == nil {
		t.Fatalf("expected Output() parse error, got out=%+v", out)
	}
}

func TestBackend_MissingRequired_SchemaError(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeMissingRequired})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	go func() {
		for range sess.Events() {
		}
	}()
	_, err = sess.Wait(ctx)
	if err == nil {
		t.Fatalf("expected wait error for missing required field")
	}
}

func TestBackend_Failure_SurfacesReason(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:          agentmtest.ModeFailure,
		FailureReason: "tests fail",
		NextLabel:     "status:failed",
	})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	go func() {
		for range sess.Events() {
		}
	}()
	_, err = sess.Wait(ctx)
	if err == nil {
		t.Fatalf("expected wait error for task failure")
	}
	extractor := sess.(interface {
		Output() (*agentm.Output, error)
	})
	out, perr := extractor.Output()
	if perr != nil {
		t.Fatalf("Output(): %v", perr)
	}
	if out.Success {
		t.Fatalf("expected success=false")
	}
	if out.FailureReason != "tests fail" {
		t.Fatalf("got failure_reason=%q", out.FailureReason)
	}
	if out.NextLabel != "status:failed" {
		t.Fatalf("got next_label=%q", out.NextLabel)
	}
}

func TestParseAndValidate_RejectsBadLabel(t *testing.T) {
	_, err := agentm.ParseAndValidate([]byte(`{"success":true,"next_label":"NotAStatus","session_log_path":"/tmp/x"}`))
	if err == nil {
		t.Fatalf("expected schema error for invalid next_label pattern")
	}
}

func TestSchemaEmbedMatchesRepo(t *testing.T) {
	// Guardrail: keep the embedded schema in lockstep with the canonical
	// schemas/agentm-output.schema.json so contributors who touch one are
	// nudged to touch the other.
	repoSchema, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "agentm-output.schema.json"))
	if err != nil {
		t.Fatalf("read canonical schema: %v", err)
	}
	embedded, err := os.ReadFile("agentm-output.schema.json")
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}
	if string(repoSchema) != string(embedded) {
		t.Fatalf("internal/agent/agentm/agentm-output.schema.json drifted from schemas/agentm-output.schema.json; please re-copy")
	}
}
