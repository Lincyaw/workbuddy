package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
)

func newTestTask(t *testing.T) *TaskContext {
	t.Helper()
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
		Repo: dir,
		Session: SessionContext{
			ID: "session-abc-123",
		},
	}
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

	expected := []string{
		"issue=42",
		"title=test issue",
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
	launcher := NewLauncher()
	task := newTestTask(t)

	agent := &config.AgentConfig{
		Name:    "codex-agent",
		Runtime: "codex",
		Command: `echo "codex output"`,
		Timeout: 10 * time.Second,
	}

	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "codex output") {
		t.Errorf("expected 'codex output' in stdout, got: %q", result.Stdout)
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
	markerPath := filepath.Join(task.Repo, "marker.txt")
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
