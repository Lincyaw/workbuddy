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

func TestCodexEventMapper(t *testing.T) {
	mapper := newCodexEventMapper("session-1")
	var seq uint64
	lines := [][]byte{
		[]byte(`{"type":"thread.started","thread_id":"thread-abc"}`),
		[]byte(`{"type":"turn.started"}`),
		[]byte(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"PONG"}}`),
		[]byte(`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc 'echo PONG'","status":"in_progress"}}`),
		[]byte(`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc 'echo PONG'","aggregated_output":"PONG\n","exit_code":0,"status":"completed"}}`),
		[]byte(`{"type":"turn.completed","usage":{"input_tokens":12,"cached_input_tokens":3,"output_tokens":4}}`),
	}
	var got []launcherevents.Event
	for _, line := range lines {
		got = append(got, mapper.Map(line, &seq)...)
	}
	if mapper.sessionRef.ID != "thread-abc" {
		t.Fatalf("session ref = %+v", mapper.sessionRef)
	}
	if len(got) != 7 {
		t.Fatalf("got %d events, want 7", len(got))
	}
	if got[0].Kind != launcherevents.KindTurnStarted {
		t.Fatalf("first kind = %s", got[0].Kind)
	}
	if got[1].Kind != launcherevents.KindAgentMessage {
		t.Fatalf("second kind = %s", got[1].Kind)
	}
	if got[2].Kind != launcherevents.KindCommandExec {
		t.Fatalf("third kind = %s", got[2].Kind)
	}
	if got[3].Kind != launcherevents.KindCommandOutput || got[4].Kind != launcherevents.KindToolResult {
		t.Fatalf("command events mismatch: %s %s", got[3].Kind, got[4].Kind)
	}
	if got[5].Kind != launcherevents.KindTokenUsage || got[6].Kind != launcherevents.KindTurnCompleted {
		t.Fatalf("tail kinds = %s %s", got[5].Kind, got[6].Kind)
	}
	var usage launcherevents.TokenUsagePayload
	if err := json.Unmarshal(got[5].Payload, &usage); err != nil {
		t.Fatalf("usage payload: %v", err)
	}
	if usage.Total != 16 || usage.Cached != 3 {
		t.Fatalf("unexpected usage: %+v", usage)
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

func TestLaunch_CodexRuntimeUsesPrompt(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}
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
		Timeout: 60 * time.Second,
	}
	result, err := launcher.Launch(context.Background(), agent, task)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if result.LastMessage != "PONG" {
		t.Fatalf("last message = %q", result.LastMessage)
	}
	if result.SessionRef.ID == "" {
		t.Fatal("expected session ref from thread.started")
	}
	if result.TokenUsage == nil || result.TokenUsage.Total == 0 {
		t.Fatalf("expected token usage, got %+v", result.TokenUsage)
	}
	if result.SessionPath == "" {
		t.Fatal("expected session path")
	}
	data, err := os.ReadFile(result.SessionPath)
	if err != nil {
		t.Fatalf("read session path: %v", err)
	}
	if !strings.Contains(string(data), `"type":"turn.completed"`) {
		t.Fatalf("expected codex jsonl artifact, got: %s", string(data))
	}
}

func TestCodexSessionRunEmitsEventsAndArtifact(t *testing.T) {
	if os.Getenv("CODEX_E2E") != "1" {
		t.Skip("set CODEX_E2E=1 to run codex runtime integration test")
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
	if result.LastMessage != "DONE" {
		t.Fatalf("last message = %q", result.LastMessage)
	}
	kinds := map[launcherevents.EventKind]bool{}
	for _, evt := range collected {
		kinds[evt.Kind] = true
	}
	for _, want := range []launcherevents.EventKind{launcherevents.KindTurnStarted, launcherevents.KindCommandExec, launcherevents.KindCommandOutput, launcherevents.KindToolResult, launcherevents.KindAgentMessage, launcherevents.KindTokenUsage, launcherevents.KindTurnCompleted} {
		if !kinds[want] {
			t.Fatalf("missing event kind %s in %v", want, kinds)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(result.SessionPath), "codex-last-message.txt")); err != nil {
		t.Fatalf("expected last message file: %v", err)
	}
}
