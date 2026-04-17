package launcher

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

func TestCodexEventMapperFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "codex-exec-events.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	mapper := newCodexEventMapper("session-1")
	var seq uint64
	var got []launcherevents.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		got = append(got, mapper.Map([]byte(line), &seq)...)
	}

	if mapper.sessionRef.ID != "thread-abc" {
		t.Fatalf("session ref = %+v", mapper.sessionRef)
	}
	if mapper.turnID != "task-123" {
		t.Fatalf("turn id = %q", mapper.turnID)
	}

	wantKinds := []launcherevents.EventKind{
		launcherevents.KindTurnStarted,
		launcherevents.KindAgentMessage,
		launcherevents.KindAgentMessage,
		launcherevents.KindReasoning,
		launcherevents.KindCommandExec,
		launcherevents.KindCommandOutput,
		launcherevents.KindToolResult,
		launcherevents.KindToolCall,
		launcherevents.KindToolResult,
		launcherevents.KindFileChange,
		launcherevents.KindTokenUsage,
		launcherevents.KindTaskComplete,
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("got %d events, want %d", len(got), len(wantKinds))
	}
	for i, want := range wantKinds {
		if got[i].Kind != want {
			t.Fatalf("event[%d] kind = %s, want %s", i, got[i].Kind, want)
		}
	}

	var usage launcherevents.TokenUsagePayload
	if err := json.Unmarshal(got[10].Payload, &usage); err != nil {
		t.Fatalf("usage payload: %v", err)
	}
	if usage.Total != 16 || usage.Cached != 2 {
		t.Fatalf("unexpected usage: %+v", usage)
	}

	var change launcherevents.FileChangePayload
	if err := json.Unmarshal(got[9].Payload, &change); err != nil {
		t.Fatalf("file change payload: %v", err)
	}
	if change.Path != "file.txt" || change.ChangeKind != "modify" {
		t.Fatalf("unexpected file change: %+v", change)
	}
	if mapper.turnCompleted == nil || mapper.turnCompleted.Status != "ok" {
		t.Fatalf("pending turn completion = %+v", mapper.turnCompleted)
	}
}

func TestCodexPromptPrefersPromptField(t *testing.T) {
	task := newTestTask(t)
	agent := &config.AgentConfig{Prompt: "issue {{.Issue.Number}}", Command: `codex exec "ignored"`}
	prompt, err := codexPrompt(agent, task)
	if err != nil {
		t.Fatalf("codexPrompt: %v", err)
	}
	if prompt != "issue 42" {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestCodexSessionSetApproverNotSupported(t *testing.T) {
	session := newCodexSession(&config.AgentConfig{}, newTestTask(t), "hello")
	if err := session.SetApprover(AlwaysAllow{}); err != ErrNotSupported {
		t.Fatalf("SetApprover error = %v", err)
	}
}

func TestLaunch_CodexRuntimeUsesPrompt(t *testing.T) {
	restore := installFakeCodex(t)
	defer restore()

	launcher := NewLauncher()
	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Runtime: config.RuntimeCodexExec,
		Prompt:  "Reply with exactly PONG",
		Policy: config.PolicyConfig{
			Sandbox:  "read-only",
			Approval: "never",
		},
		Timeout: 10 * time.Second,
	}
	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if result.LastMessage != "PONG" {
		t.Fatalf("last message = %q", result.LastMessage)
	}
	if result.SessionRef.ID != "thread-abc" {
		t.Fatalf("session ref = %+v", result.SessionRef)
	}
	if result.TokenUsage == nil || result.TokenUsage.Total != 16 {
		t.Fatalf("expected token usage, got %+v", result.TokenUsage)
	}
	if result.SessionPath == "" {
		t.Fatal("expected session path")
	}
	data, err := os.ReadFile(result.SessionPath)
	if err != nil {
		t.Fatalf("read session path: %v", err)
	}
	if !strings.Contains(string(data), `"type":"task_complete"`) {
		t.Fatalf("expected codex jsonl artifact, got: %s", string(data))
	}
}

func TestCodexSessionRunEmitsEventsAndArtifact(t *testing.T) {
	restore := installFakeCodex(t)
	defer restore()

	launcher := NewLauncher()
	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Runtime: config.RuntimeCodexExec,
		Prompt:  "Run `echo PONG` and then reply DONE.",
		Policy:  config.PolicyConfig{Sandbox: "danger-full-access", Approval: "on-request"},
		Timeout: 10 * time.Second,
	}
	session, err := launcher.Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = session.Close() }()

	ch := make(chan launcherevents.Event, 32)
	var collected []launcherevents.Event
	done := make(chan struct{})
	go func() {
		for evt := range ch {
			collected = append(collected, evt)
		}
		close(done)
	}()

	result, err := session.Run(context.Background(), ch)
	close(ch)
	<-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.LastMessage != "PONG" {
		t.Fatalf("last message = %q", result.LastMessage)
	}
	wantDir := filepath.Join(task.RepoRoot, ".workbuddy", "sessions", task.Session.ID)
	if filepath.Dir(result.SessionPath) != wantDir {
		t.Fatalf("session path dir = %q, want %q", filepath.Dir(result.SessionPath), wantDir)
	}
	if strings.HasPrefix(result.SessionPath, task.WorkDir) {
		t.Fatalf("session path should not live under workdir: %q", result.SessionPath)
	}

	kinds := map[launcherevents.EventKind]bool{}
	for _, evt := range collected {
		kinds[evt.Kind] = true
	}
	for _, want := range []launcherevents.EventKind{
		launcherevents.KindTurnStarted,
		launcherevents.KindCommandExec,
		launcherevents.KindCommandOutput,
		launcherevents.KindToolResult,
		launcherevents.KindAgentMessage,
		launcherevents.KindTokenUsage,
		launcherevents.KindTurnCompleted,
	} {
		if !kinds[want] {
			t.Fatalf("missing event kind %s in %v", want, kinds)
		}
	}

	lastMsgPath := filepath.Join(filepath.Dir(result.SessionPath), "codex-last-message.txt")
	if _, err := os.Stat(lastMsgPath); err != nil {
		t.Fatalf("expected last message file: %v", err)
	}
}

func TestCodexSessionRunKillsHungProcessAfterTaskComplete(t *testing.T) {
	restore := installFakeCodexScript(t, "#!/bin/sh\n"+
		"output_last_message=''\n"+
		"while [ $# -gt 0 ]; do\n"+
		"  case \"$1\" in\n"+
		"    --output-last-message)\n"+
		"      output_last_message=\"$2\"\n"+
		"      shift 2\n"+
		"      ;;\n"+
		"    *)\n"+
		"      shift\n"+
		"      ;;\n"+
		"  esac\n"+
		"done\n"+
		"if [ -n \"$output_last_message\" ]; then\n"+
		"  mkdir -p \"$(dirname \"$output_last_message\")\"\n"+
		"  printf 'DONE\\n' > \"$output_last_message\"\n"+
		"fi\n"+
		"printf '{\"type\":\"task_started\",\"task_id\":\"task-123\"}\\n'\n"+
		"printf '{\"type\":\"task_complete\"}\\n'\n"+
		"while :; do sleep 1; done\n")
	defer restore()

	oldGrace := codexTaskCompleteGracePeriod
	oldKillDelay := codexTaskCompleteKillDelay
	codexTaskCompleteGracePeriod = 50 * time.Millisecond
	codexTaskCompleteKillDelay = 50 * time.Millisecond
	defer func() {
		codexTaskCompleteGracePeriod = oldGrace
		codexTaskCompleteKillDelay = oldKillDelay
	}()

	launcher := NewLauncher()
	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Runtime: config.RuntimeCodexExec,
		Prompt:  "Reply DONE",
		Timeout: 5 * time.Second,
	}

	session, err := launcher.Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = session.Close() }()

	ch := make(chan launcherevents.Event, 16)
	var collected []launcherevents.Event
	done := make(chan struct{})
	go func() {
		for evt := range ch {
			collected = append(collected, evt)
		}
		close(done)
	}()

	start := time.Now()
	result, err := session.Run(context.Background(), ch)
	close(ch)
	<-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("expected forced termination to produce non-zero exit, got %d", result.ExitCode)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("expected hung codex to be terminated quickly, took %s", elapsed)
	}

	turnCompleted := 0
	for _, evt := range collected {
		if evt.Kind == launcherevents.KindTurnCompleted {
			turnCompleted++
		}
	}
	if turnCompleted != 1 {
		t.Fatalf("turn.completed count = %d, want 1", turnCompleted)
	}
}

func TestNewCodexSessionFallsBackToWorkDirWhenRepoRootEmpty(t *testing.T) {
	task := newTestTask(t)
	task.RepoRoot = ""

	session := newCodexSession(&config.AgentConfig{}, task, "hello")
	wantDir := filepath.Join(task.WorkDir, ".workbuddy", "sessions", task.Session.ID)
	if filepath.Dir(session.stdoutPath) != wantDir {
		t.Fatalf("stdout path dir = %q, want %q", filepath.Dir(session.stdoutPath), wantDir)
	}
}

func TestCodexExecE2E(t *testing.T) {
	if os.Getenv("CODEX_E2E") != "1" {
		t.Skip("set CODEX_E2E=1 to run codex runtime e2e")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}

	launcher := NewLauncher()
	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Runtime: config.RuntimeCodexExec,
		Prompt:  "Run `echo PONG` and then reply DONE.",
		Policy:  config.PolicyConfig{Sandbox: "danger-full-access", Approval: "never"},
		Timeout: 90 * time.Second,
	}
	session, err := launcher.Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = session.Close() }()

	ch := make(chan launcherevents.Event, 64)
	var collected []launcherevents.Event
	done := make(chan struct{})
	go func() {
		for evt := range ch {
			collected = append(collected, evt)
		}
		close(done)
	}()

	result, err := session.Run(context.Background(), ch)
	close(ch)
	<-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.LastMessage == "" {
		t.Fatal("expected last message")
	}
	if len(collected) == 0 {
		t.Fatal("expected event stream")
	}
}

func TestCodexSessionRunSynthesizesTurnCompletedOnCrash(t *testing.T) {
	restore := installFakeCodexScript(t, "#!/bin/sh\nprintf 'boom\\n' >&2\nexit 3\n")
	defer restore()

	launcher := NewLauncher()
	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Runtime: config.RuntimeCodexExec,
		Prompt:  "irrelevant",
		Policy:  config.PolicyConfig{Sandbox: "danger-full-access", Approval: "on-request"},
		Timeout: 5 * time.Second,
	}
	session, err := launcher.Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = session.Close() }()

	ch := make(chan launcherevents.Event, 16)
	var collected []launcherevents.Event
	done := make(chan struct{})
	go func() {
		for evt := range ch {
			collected = append(collected, evt)
		}
		close(done)
	}()

	result, err := session.Run(context.Background(), ch)
	close(ch)
	<-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", result.ExitCode)
	}

	var sawError, sawTurnCompleted bool
	for _, evt := range collected {
		switch evt.Kind {
		case launcherevents.KindError:
			sawError = true
		case launcherevents.KindTurnCompleted:
			sawTurnCompleted = true
		}
	}
	if !sawError {
		t.Fatalf("expected synthetic error event on non-zero exit without task_complete, got %v", collected)
	}
	if !sawTurnCompleted {
		t.Fatalf("expected synthetic turn.completed event when codex crashed without task_complete, got %v", collected)
	}
}

func TestLaunch_CodexOutputContractValidatesLastMessage(t *testing.T) {
	restore := installFakeCodexLastMessage(t, `{"status":"ok"}`)
	defer restore()

	launcher := NewLauncher()
	task := newTestTask(t)
	agentDir := t.TempDir()
	writeOutputSchema(t, agentDir)

	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Runtime: config.RuntimeCodexExec,
		Prompt:  "return json",
		Policy:  config.PolicyConfig{Sandbox: "read-only", Approval: "never"},
		OutputContract: config.OutputContractConfig{
			SchemaFile: "schemas/result.json",
		},
		SourcePath: filepath.Join(agentDir, "agent.md"),
		Timeout:    10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if result.LastMessage != `{"status":"ok"}` {
		t.Fatalf("last message = %q", result.LastMessage)
	}
}

func TestLaunch_CodexOutputContractRejectsInvalidLastMessage(t *testing.T) {
	restore := installFakeCodexLastMessage(t, `{"missing":"status"}`)
	defer restore()

	launcher := NewLauncher()
	task := newTestTask(t)
	agentDir := t.TempDir()
	writeOutputSchema(t, agentDir)

	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Runtime: config.RuntimeCodexExec,
		Prompt:  "return json",
		Policy:  config.PolicyConfig{Sandbox: "read-only", Approval: "never"},
		OutputContract: config.OutputContractConfig{
			SchemaFile: "schemas/result.json",
		},
		SourcePath: filepath.Join(agentDir, "agent.md"),
		Timeout:    10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err == nil {
		t.Fatal("expected output contract validation error")
	}
	if result == nil || result.ExitCode != 0 {
		t.Fatalf("expected successful codex result, got %+v", result)
	}
}

func TestCodexSessionRun_EmitsErrorTerminalEventOnOutputContractFailure(t *testing.T) {
	restore := installFakeCodexLastMessage(t, `{"missing":"status"}`)
	defer restore()

	task := newTestTask(t)
	agentDir := t.TempDir()
	writeOutputSchema(t, agentDir)

	session := newCodexSession(&config.AgentConfig{
		Name:    "codex-agent",
		Runtime: config.RuntimeCodexExec,
		Prompt:  "return json",
		Policy:  config.PolicyConfig{Sandbox: "read-only", Approval: "never"},
		OutputContract: config.OutputContractConfig{
			SchemaFile: "schemas/result.json",
		},
		SourcePath: filepath.Join(agentDir, "agent.md"),
		Timeout:    10 * time.Second,
	}, task, "return json")

	events, result, err := collectSessionEvents(t, session)
	if err == nil {
		t.Fatal("expected output contract validation error")
	}
	if result == nil || result.ExitCode != 0 {
		t.Fatalf("expected successful codex result, got %+v", result)
	}
	if got := turnCompletedStatuses(t, events); len(got) != 1 || got[0] != "error" {
		t.Fatalf("turn.completed statuses = %v, want [error]", got)
	}
	if got := eventErrorCodes(t, events); len(got) != 1 || got[0] != "output_contract" {
		t.Fatalf("error codes = %v, want [output_contract]", got)
	}
}

func installFakeCodex(t *testing.T) func() {
	t.Helper()

	fixturePath := filepath.Join(t.TempDir(), "codex-exec-events.jsonl")
	data, err := os.ReadFile(filepath.Join("testdata", "codex-exec-events.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(fixturePath, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "codex")
	script := "#!/bin/sh\n" +
		"output_last_message=''\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --output-last-message)\n" +
		"      output_last_message=\"$2\"\n" +
		"      shift 2\n" +
		"      ;;\n" +
		"    *)\n" +
		"      shift\n" +
		"      ;;\n" +
		"  esac\n" +
		"done\n" +
		"if [ -n \"$output_last_message\" ]; then\n" +
		"  mkdir -p \"$(dirname \"$output_last_message\")\"\n" +
		"  printf 'PONG\\n' > \"$output_last_message\"\n" +
		"fi\n" +
		"cat \"" + fixturePath + "\"\n" +
		"printf 'codex stderr for testing\\n' >&2\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	return func() {
		_ = os.Setenv("PATH", oldPath)
	}
}

func installFakeCodexLastMessage(t *testing.T, lastMessage string) func() {
	t.Helper()

	fixturePath := filepath.Join(t.TempDir(), "codex-exec-events.jsonl")
	data, err := os.ReadFile(filepath.Join("testdata", "codex-exec-events.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(fixturePath, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "codex")
	script := "#!/bin/sh\n" +
		"output_last_message=''\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --output-last-message)\n" +
		"      output_last_message=\"$2\"\n" +
		"      shift 2\n" +
		"      ;;\n" +
		"    *)\n" +
		"      shift\n" +
		"      ;;\n" +
		"  esac\n" +
		"done\n" +
		"if [ -n \"$output_last_message\" ]; then\n" +
		"  mkdir -p \"$(dirname \"$output_last_message\")\"\n" +
		"  printf '%s\\n' '" + strings.ReplaceAll(lastMessage, "'", "'\"'\"'") + "' > \"$output_last_message\"\n" +
		"fi\n" +
		"cat \"" + fixturePath + "\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	return func() {
		_ = os.Setenv("PATH", oldPath)
	}
}

func installFakeCodexScript(t *testing.T, script string) func() {
	t.Helper()

	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	return func() {
		_ = os.Setenv("PATH", oldPath)
	}
}
