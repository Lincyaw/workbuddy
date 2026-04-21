package launcher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

func newTestTask(t *testing.T) *TaskContext {
	t.Helper()
	repoRoot := t.TempDir()
	dir := t.TempDir()
	return &TaskContext{
		Issue: IssueContext{
			Number: 42,
			Title:  "test issue",
			Body:   "test body",
			Labels: []string{"status:dev", "priority:high"},
		},
		PR: PRContext{
			URL:    "https://github.com/test/repo/pull/1",
			Branch: "feat/test",
		},
		Repo:     "test/repo",
		RepoRoot: repoRoot,
		WorkDir:  dir,
		Session: SessionContext{
			ID: "session-abc-123",
		},
	}
}

func writeOutputSchema(t *testing.T, dir string) string {
	t.Helper()
	schemaDir := filepath.Join(dir, "schemas")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatalf("mkdir schema dir: %v", err)
	}
	schemaPath := filepath.Join(schemaDir, "result.json")
	schema := `{
  "type": "object",
  "required": ["status"],
  "properties": {
    "status": {"type": "string"}
  }
}`
	if err := os.WriteFile(schemaPath, []byte(schema), 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	return schemaPath
}

func collectSessionEvents(t *testing.T, session Session) ([]launcherevents.Event, *Result, error) {
	t.Helper()
	ch := make(chan launcherevents.Event, 32)
	var events []launcherevents.Event
	done := make(chan struct{})
	go func() {
		for evt := range ch {
			events = append(events, evt)
		}
		close(done)
	}()
	result, err := session.Run(context.Background(), ch)
	close(ch)
	<-done
	return events, result, err
}

func turnCompletedStatuses(t *testing.T, events []launcherevents.Event) []string {
	t.Helper()
	var statuses []string
	for _, evt := range events {
		if evt.Kind != launcherevents.KindTurnCompleted {
			continue
		}
		var payload launcherevents.TurnCompletedPayload
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			t.Fatalf("unmarshal turn.completed payload: %v", err)
		}
		statuses = append(statuses, payload.Status)
	}
	return statuses
}

func eventErrorCodes(t *testing.T, events []launcherevents.Event) []string {
	t.Helper()
	var codes []string
	for _, evt := range events {
		if evt.Kind != launcherevents.KindError {
			continue
		}
		var payload launcherevents.ErrorPayload
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			t.Fatalf("unmarshal error payload: %v", err)
		}
		codes = append(codes, payload.Code)
	}
	return codes
}

// Test 1: Normal execution — command runs and returns stdout, stderr, exit code 0
func TestLaunch_NormalExec(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "test-agent",
		Runtime: "claude-code",
		Command: `echo "hello world"`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello world") {
		t.Errorf("expected stdout to contain 'hello world', got: %q", result.Stdout)
	}
	if result.Duration <= 0 {
		t.Error("expected positive duration")
	}
}

// Test 2: Timeout — subprocess killed after timeout
func TestLaunch_Timeout(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "timeout-agent",
		Runtime: "claude-code",
		Command: "sleep 60",
		Timeout: 500 * time.Millisecond,
	}

	_, err := launcher.Launch(context.Background(), agent, task)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout in error message, got: %v", err)
	}
}

// Test 3: Template rendering — variables are expanded in command
func TestLaunch_TemplateRender(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "template-agent",
		Runtime: "claude-code",
		Command: `echo "issue={{.Issue.Number}} title={{.Issue.Title}} repo={{.Repo}} session={{.Session.ID}} pr={{.PR.URL}} branch={{.PR.Branch}}"`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Issue.Title is auto-escaped (wrapped in single quotes) for shell safety.
	// Non-issue fields (Repo, Session.ID, PR.*) are not escaped.
	expected := []string{
		"issue=42",
		"title='test issue'",
		"repo=" + task.Repo,
		"session=session-abc-123",
		"pr=https://github.com/test/repo/pull/1",
		"branch=feat/test",
	}
	for _, exp := range expected {
		if !strings.Contains(result.Stdout, exp) {
			t.Errorf("expected stdout to contain %q, got: %q", exp, result.Stdout)
		}
	}
}

// Test 4: Context cancellation — subprocess killed when context is cancelled
func TestLaunch_Cancel(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "cancel-agent",
		Runtime: "claude-code",
		Command: "sleep 60",
		Timeout: 30 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	_, err := launcher.Launch(ctx, agent, task)
	if err == nil {
		t.Fatal("expected cancel error, got nil")
	}
	if !strings.Contains(err.Error(), "cancel") && !strings.Contains(err.Error(), "signal") {
		t.Errorf("expected cancel-related error, got: %v", err)
	}
}

func TestStart_GitHubActionsRunnerUsesRemoteSession(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "remote-agent",
		Runner:  config.RunnerGitHubActions,
		Runtime: config.RuntimeCodex,
		Prompt:  "remote",
		GitHubActions: config.GitHubActionsRunnerConfig{
			Workflow:     "workbuddy-remote-runner.yml",
			Ref:          "main",
			PollInterval: time.Millisecond,
		},
		Timeout: time.Minute,
	}

	session, err := launcher.Start(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = session.Close() }()
	remote, ok := session.(*runtimepkg.GHASession)
	if !ok {
		t.Fatalf("session type = %T, want *runtime.GHASession", session)
	}
	if remote.Client == nil {
		t.Fatal("expected GitHub Actions client")
	}
}

// Test 5: Meta parse success — WORKBUDDY_META block is extracted from stdout
func TestLaunch_MetaParseSuccess(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "meta-agent",
		Runtime: "claude-code",
		Command: `echo "some output" && echo "WORKBUDDY_META_BEGIN" && echo '{"pr_url":"https://github.com/test/repo/pull/99","branch":"feat/xxx","commit_sha":"abc123","summary":"implemented feature X"}' && echo "WORKBUDDY_META_END" && echo "more output"`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Meta == nil {
		t.Fatal("expected Meta to be non-nil")
	}
	if result.Meta["pr_url"] != "https://github.com/test/repo/pull/99" {
		t.Errorf("unexpected pr_url: %s", result.Meta["pr_url"])
	}
	if result.Meta["branch"] != "feat/xxx" {
		t.Errorf("unexpected branch: %s", result.Meta["branch"])
	}
	if result.Meta["commit_sha"] != "abc123" {
		t.Errorf("unexpected commit_sha: %s", result.Meta["commit_sha"])
	}
	if result.Meta["summary"] != "implemented feature X" {
		t.Errorf("unexpected summary: %s", result.Meta["summary"])
	}
}

// Test 6: Meta missing — no WORKBUDDY_META block, Meta should be nil (no error)
func TestLaunch_MetaMissing(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "no-meta-agent",
		Runtime: "claude-code",
		Command: `echo "just regular output"`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Meta != nil {
		t.Errorf("expected Meta to be nil, got: %v", result.Meta)
	}
}

// Test 7: claude-code runtime — verify the runtime is registered and works
func TestLaunch_ClaudeCodeRuntime(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "claude-agent",
		Runtime: "claude-code",
		Command: `echo "claude output"`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "claude output") {
		t.Errorf("expected 'claude output' in stdout, got: %q", result.Stdout)
	}
}

// Test 8: codex runtime — verify the runtime is registered and works
func TestLaunch_CodexRuntime(t *testing.T) {
	restore := installFakeCodex(t)
	defer restore()

	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Runtime: "codex",
		Prompt:  "Reply with exactly PONG",
		Policy: config.PolicyConfig{
			Sandbox:  "read-only",
			Approval: "never",
		},
		Timeout: 30 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.LastMessage != "PONG" {
		t.Errorf("expected last message 'PONG', got: %q", result.LastMessage)
	}
}

func TestLaunch_OutputContractValidatesProcessOutput(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)
	agentDir := t.TempDir()
	schemaPath := writeOutputSchema(t, agentDir)

	agent := &config.AgentConfig{
		Name:    "structured-agent",
		Runtime: "claude-code",
		Command: `printf '{"status":"ok"}'`,
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
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
	if agent.OutputContractSchemaPath() != schemaPath {
		t.Fatalf("schema path = %q, want %q", agent.OutputContractSchemaPath(), schemaPath)
	}
}

func TestLaunch_OutputContractRejectsInvalidProcessOutput(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)
	agentDir := t.TempDir()
	writeOutputSchema(t, agentDir)

	agent := &config.AgentConfig{
		Name:    "structured-agent",
		Runtime: "claude-code",
		Command: `printf '{"missing":"status"}'`,
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
		t.Fatalf("expected successful process result, got %+v", result)
	}
	if !strings.Contains(err.Error(), "output_contract") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProcessSessionRun_EmitsErrorTerminalEventOnOutputContractFailure(t *testing.T) {
	task := newTestTask(t)
	agentDir := t.TempDir()
	writeOutputSchema(t, agentDir)

	session := newProcessSession(config.RuntimeClaudeCode, &config.AgentConfig{
		Name:    "structured-agent",
		Runtime: config.RuntimeClaudeCode,
		Command: `printf '{"missing":"status"}'`,
		OutputContract: config.OutputContractConfig{
			SchemaFile: "schemas/result.json",
		},
		SourcePath: filepath.Join(agentDir, "agent.md"),
		Timeout:    10 * time.Second,
	}, task, nil)

	events, result, err := collectSessionEvents(t, session)
	if err == nil {
		t.Fatal("expected output contract validation error")
	}
	if result == nil || result.ExitCode != 0 {
		t.Fatalf("expected successful process result, got %+v", result)
	}
	if got := turnCompletedStatuses(t, events); len(got) != 1 || got[0] != "error" {
		t.Fatalf("turn.completed statuses = %v, want [error]", got)
	}
	if got := eventErrorCodes(t, events); len(got) != 1 || got[0] != "output_contract" {
		t.Fatalf("error codes = %v, want [output_contract]", got)
	}
}

// Test 9: Unknown runtime — clear error message
func TestLaunch_UnknownRuntime(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "bad-agent",
		Runtime: "gpt-pilot",
		Command: `echo "should not run"`,
		Timeout: 10 * time.Second,
	}

	_, err := launcher.Launch(context.Background(), agent, task)
	if err == nil {
		t.Fatal("expected error for unknown runtime")
	}
	if !strings.Contains(err.Error(), "unsupported runtime: gpt-pilot") {
		t.Errorf("expected 'unsupported runtime: gpt-pilot' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "claude-code") || !strings.Contains(err.Error(), "codex") {
		t.Errorf("expected supported runtimes listed in error, got: %v", err)
	}
}

// Test: default runtime is claude-code when not specified
func TestLaunch_DefaultRuntime(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "default-agent",
		Runtime: "", // empty → default claude-code
		Command: `echo "default runtime"`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "default runtime") {
		t.Errorf("expected 'default runtime' in stdout, got: %q", result.Stdout)
	}
}

// Test: environment variables are set correctly
func TestLaunch_EnvVars(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "env-agent",
		Runtime: "claude-code",
		Command: `echo "NUM=$WORKBUDDY_ISSUE_NUMBER TITLE=$WORKBUDDY_ISSUE_TITLE BODY=$WORKBUDDY_ISSUE_BODY REPO=$WORKBUDDY_REPO SID=$WORKBUDDY_SESSION_ID"`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{
		"NUM=42",
		"TITLE=test issue",
		"BODY=test body",
		"REPO=" + task.Repo,
		"SID=session-abc-123",
	}
	for _, exp := range expected {
		if !strings.Contains(result.Stdout, exp) {
			t.Errorf("expected stdout to contain %q, got: %q", exp, result.Stdout)
		}
	}
}

// Test: non-zero exit code is captured
func TestLaunch_NonZeroExit(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "fail-agent",
		Runtime: "claude-code",
		Command: `echo "failing" && exit 2`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 2 {
		t.Errorf("expected exit code 2, got %d", result.ExitCode)
	}
}

// Test: command working directory is set to task.Repo
func TestLaunch_WorkingDirectory(t *testing.T) {
	launcher := NewLauncher()
	task := newTestTask(t)

	// Create a marker file in the temp dir
	markerPath := filepath.Join(task.WorkDir, "marker.txt")
	if err := os.WriteFile(markerPath, []byte("found"), 0644); err != nil {
		t.Fatal(err)
	}

	agent := &config.AgentConfig{
		Name:    "dir-agent",
		Runtime: "claude-code",
		Command: `cat marker.txt`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "found") {
		t.Errorf("expected 'found' in stdout (working dir test), got: %q", result.Stdout)
	}
}

// Test: parseMeta unit tests
func TestParseMeta(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantNil  bool
		wantKeys []string
	}{
		{
			name:    "missing markers",
			input:   "just some output",
			wantNil: true,
		},
		{
			name:    "only begin marker",
			input:   "WORKBUDDY_META_BEGIN\n{}\n",
			wantNil: true,
		},
		{
			name:    "invalid JSON",
			input:   "WORKBUDDY_META_BEGIN\nnot json\nWORKBUDDY_META_END",
			wantNil: true,
		},
		{
			name:     "valid meta",
			input:    "output\nWORKBUDDY_META_BEGIN\n{\"pr_url\":\"https://example.com\",\"branch\":\"feat/x\"}\nWORKBUDDY_META_END\nmore",
			wantNil:  false,
			wantKeys: []string{"pr_url", "branch"},
		},
		{
			name:    "empty JSON block",
			input:   "WORKBUDDY_META_BEGIN\n\nWORKBUDDY_META_END",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMeta(tt.input)
			if tt.wantNil && got != nil {
				t.Errorf("expected nil, got %v", got)
			}
			if !tt.wantNil && got == nil {
				t.Error("expected non-nil meta")
			}
			for _, key := range tt.wantKeys {
				if _, ok := got[key]; !ok {
					t.Errorf("expected key %q in meta", key)
				}
			}
		})
	}
}

// Test: extractPrompt detects claude -p commands and extracts the prompt
func TestExtractPrompt(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantOK     bool
		wantPrompt string
		wantArgs   []string
	}{
		{
			name:       "double quoted",
			input:      `claude -p "hello world"`,
			wantOK:     true,
			wantPrompt: "hello world",
		},
		{
			name:       "single quoted",
			input:      `claude -p 'hello world'`,
			wantOK:     true,
			wantPrompt: "hello world",
		},
		{
			name:       "with inner quotes",
			input:      `claude -p "say 'hi' to everyone"`,
			wantOK:     true,
			wantPrompt: "say 'hi' to everyone",
		},
		{
			name:       "multiline prompt",
			input:      "claude -p \"line1\nline2\nline3\"",
			wantOK:     true,
			wantPrompt: "line1\nline2\nline3",
		},
		{
			name:       "with backticks in prompt",
			input:      "claude -p \"run `echo hello`\"",
			wantOK:     true,
			wantPrompt: "run `echo hello`",
		},
		{
			name:       "with --print flag preserved",
			input:      `claude --print -p "hello world"`,
			wantOK:     true,
			wantPrompt: "hello world",
			wantArgs:   []string{"--print"},
		},
		{
			name:       "with extra flags before prompt",
			input:      `claude --model sonnet --output-format stream-json -p "hello world"`,
			wantOK:     true,
			wantPrompt: "hello world",
			wantArgs:   []string{"--model", "sonnet", "--output-format", "stream-json"},
		},
		{
			name:   "not a claude command",
			input:  `echo "hello world"`,
			wantOK: false,
		},
		{
			name:   "claude without -p",
			input:  `claude --help`,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt, args, ok := extractPrompt(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("extractPrompt ok=%v, want %v", ok, tt.wantOK)
			}
			if ok && prompt != tt.wantPrompt {
				t.Errorf("extractPrompt prompt=%q, want %q", prompt, tt.wantPrompt)
			}
			if ok && len(tt.wantArgs) > 0 {
				if len(args) != len(tt.wantArgs) {
					t.Fatalf("extractPrompt args=%v, want %v", args, tt.wantArgs)
				}
				for i, a := range args {
					if a != tt.wantArgs[i] {
						t.Errorf("extractPrompt args[%d]=%q, want %q", i, a, tt.wantArgs[i])
					}
				}
			}
		})
	}
}

// Test: renderCommand unit tests
func TestRenderCommand(t *testing.T) {
	task := &TaskContext{
		Issue: IssueContext{
			Number: 7,
			Title:  "my issue",
			Body:   "details",
			Labels: []string{"bug"},
		},
		PR: PRContext{
			URL:    "https://github.com/owner/repo/pull/7",
			Branch: "fix/7",
		},
		Repo: "/tmp/repo",
		Session: SessionContext{
			ID: "sess-1",
		},
	}

	rendered, err := renderCommand("echo {{.Issue.Number}} {{.Session.ID}}", task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rendered != "echo 7 sess-1" {
		t.Errorf("unexpected rendered command: %q", rendered)
	}
}

// Test: shellEscape correctly wraps values
func TestShellEscape(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "'hello'"},
		{"it's", `'it'\''s'`},
		{`"; rm -rf / #`, `'"; rm -rf / #'`},
		{"", "''"},
		{"a'b'c", `'a'\''b'\''c'`},
	}
	for _, tt := range tests {
		got := shellEscape(tt.input)
		if got != tt.want {
			t.Errorf("shellEscape(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// Test: renderCommand auto-escapes user-controlled issue fields
func TestRenderCommand_AutoEscapesIssueFields(t *testing.T) {
	task := &TaskContext{
		Issue: IssueContext{
			Number: 1,
			Title:  `"; rm -rf / #`,
			Body:   "$(evil command)",
			Labels: []string{"label; whoami"},
		},
		Repo:    "owner/repo",
		Session: SessionContext{ID: "s1"},
	}

	rendered, err := renderCommand("echo {{.Issue.Title}}", task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The title must be wrapped in single quotes, neutralizing the shell metacharacters
	if !strings.Contains(rendered, "'") {
		t.Errorf("expected shell-escaped title with single quotes, got: %q", rendered)
	}
	if strings.Contains(rendered, "rm -rf") && !strings.Contains(rendered, "'") {
		t.Error("shell metacharacters in title are not escaped")
	}
}

// Test: shell injection via issue title is neutralized end-to-end
func TestLaunch_ShellInjectionTitle(t *testing.T) {
	launcher := NewLauncher()
	dir := t.TempDir()

	// A malicious title that tries to create a file via command injection
	markerFile := filepath.Join(dir, "pwned.txt")

	task := &TaskContext{
		Issue: IssueContext{
			Number: 99,
			Title:  `"; touch ` + markerFile + ` #`,
			Body:   "normal body",
			Labels: []string{"status:dev"},
		},
		Repo:    "test/repo",
		WorkDir: dir,
		Session: SessionContext{ID: "sec-test"},
	}

	agent := &config.AgentConfig{
		Name:    "injection-test",
		Runtime: "claude-code",
		Command: `echo {{.Issue.Title}}`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The command should have printed the escaped title, not executed injection
	if result.ExitCode != 0 {
		t.Logf("stderr: %s", result.Stderr)
	}

	// The marker file must NOT exist — injection must have been prevented
	if _, err := os.Stat(markerFile); err == nil {
		t.Fatal("SECURITY: shell injection succeeded — marker file was created")
	}
}

// Test: shell injection via issue body is neutralized end-to-end
func TestLaunch_ShellInjectionBody(t *testing.T) {
	launcher := NewLauncher()
	dir := t.TempDir()

	markerFile := filepath.Join(dir, "pwned_body.txt")

	task := &TaskContext{
		Issue: IssueContext{
			Number: 100,
			Title:  "safe title",
			Body:   "$(touch " + markerFile + ")",
			Labels: []string{"status:dev"},
		},
		Repo:    "test/repo",
		WorkDir: dir,
		Session: SessionContext{ID: "sec-test-2"},
	}

	agent := &config.AgentConfig{
		Name:    "injection-body-test",
		Runtime: "claude-code",
		Command: `echo {{.Issue.Body}}`,
		Timeout: 10 * time.Second,
	}

	_, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(markerFile); err == nil {
		t.Fatal("SECURITY: shell injection via body succeeded — marker file was created")
	}
}

// Test: shellEscape template function is available for explicit use
func TestRenderCommand_ShellEscapeFunc(t *testing.T) {
	task := &TaskContext{
		Issue:   IssueContext{Number: 1, Title: "test"},
		Repo:    "owner/repo",
		Session: SessionContext{ID: "s1"},
	}

	rendered, err := renderCommand(`echo {{shellEscape .Repo}}`, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rendered != "echo 'owner/repo'" {
		t.Errorf("unexpected rendered: %q", rendered)
	}
}
