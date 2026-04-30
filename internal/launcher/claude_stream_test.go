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
)

func TestClaudeStreamEventMapperMapsStructuredEvents(t *testing.T) {
	lines := []string{
		`{"type":"system.init","session_id":"claude-session-1"}`,
		`{"type":"assistant.message_start","message":{"id":"msg-1"}}`,
		`{"type":"assistant.content_block_delta","delta":{"type":"thinking_delta","thinking":"inspect repo"}}`,
		`{"type":"assistant.content_block_delta","delta":{"type":"text_delta","text":"Running checks"}}`,
		`{"type":"assistant.content_block_start","content_block":{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"git status","cwd":"/repo"}}}`,
		`{"type":"user.tool_result","tool_use_id":"tool-1","is_error":false,"content":"On branch main"}`,
		`{"type":"assistant.content_block_start","content_block":{"type":"tool_use","id":"tool-2","name":"Write","input":{"file_path":"docs/out.md","content":"hello"}}}`,
		`{"type":"user.tool_result","tool_use_id":"tool-2","is_error":false,"content":"wrote file"}`,
		`{"type":"assistant.content_block_delta","delta":{"type":"text_delta","text":"Done"}}`,
		`{"type":"assistant.message_stop","usage":{"input_tokens":12,"output_tokens":5,"cache_read_input_tokens":3}}`,
	}

	mapper := newClaudeStreamEventMapper("session-123")
	var seq uint64
	var got []launcherevents.Event
	for _, line := range lines {
		got = append(got, mapper.Map([]byte(line), &seq)...)
	}

	wantKinds := []launcherevents.EventKind{
		launcherevents.KindTurnStarted,
		launcherevents.KindReasoning,
		launcherevents.KindAgentMessage,
		launcherevents.KindToolCall,
		launcherevents.KindCommandExec,
		launcherevents.KindToolResult,
		launcherevents.KindCommandOutput,
		launcherevents.KindToolCall,
		launcherevents.KindToolResult,
		launcherevents.KindFileChange,
		launcherevents.KindAgentMessage,
		launcherevents.KindTokenUsage,
		launcherevents.KindTurnCompleted,
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("got %d events, want %d", len(got), len(wantKinds))
	}
	for i, want := range wantKinds {
		if got[i].Kind != want {
			t.Fatalf("event[%d] kind = %s, want %s", i, got[i].Kind, want)
		}
	}
	if mapper.SessionRefValue.ID != "claude-session-1" {
		t.Fatalf("session ref = %+v", mapper.SessionRefValue)
	}
	if mapper.LastMessageValue != "Running checksDone" {
		t.Fatalf("last message = %q", mapper.LastMessageValue)
	}

	var execPayload launcherevents.CommandExecPayload
	if err := json.Unmarshal(got[4].Payload, &execPayload); err != nil {
		t.Fatalf("command exec payload: %v", err)
	}
	if strings.Join(execPayload.Cmd, " ") != "git status" {
		t.Fatalf("command exec = %+v", execPayload)
	}

	var filePayload launcherevents.FileChangePayload
	if err := json.Unmarshal(got[9].Payload, &filePayload); err != nil {
		t.Fatalf("file change payload: %v", err)
	}
	if filePayload.Path != "docs/out.md" || filePayload.ChangeKind != "create" {
		t.Fatalf("file change = %+v", filePayload)
	}
}

func TestProcessSessionRun_ClaudePromptEmitsStructuredEvents(t *testing.T) {
	restore := installFakeClaude(t, []string{
		`{"type":"system.init","session_id":"claude-session-1"}`,
		`{"type":"assistant.content_block_delta","delta":{"type":"text_delta","text":"Plan:"}}`,
		`{"type":"assistant.content_block_start","content_block":{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"echo PONG","cwd":"/repo"}}}`,
		`{"type":"user.tool_result","tool_use_id":"tool-1","is_error":false,"content":"PONG\n"}`,
		`{"type":"assistant.content_block_delta","delta":{"type":"text_delta","text":" done"}}`,
		`{"type":"assistant.message_stop","usage":{"input_tokens":9,"output_tokens":4,"cache_read_input_tokens":1}}`,
	})
	defer restore()

	launcher := newTestLauncher(t)
	task := newTestTask(t)
	agent := &config.AgentConfig{
		Name:    "claude-agent",
		Runtime: config.RuntimeClaudeCode,
		Prompt:  "Say hi",
		Policy: config.PolicyConfig{
			Sandbox: "read-only",
			Model:   "sonnet",
		},
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
	if result.LastMessage != "Plan: done" {
		t.Fatalf("last message = %q", result.LastMessage)
	}
	if result.TokenUsage == nil || result.TokenUsage.Total != 13 {
		t.Fatalf("token usage = %+v", result.TokenUsage)
	}
	if result.SessionRef.ID != "claude-session-1" {
		t.Fatalf("session ref = %+v", result.SessionRef)
	}

	kinds := map[launcherevents.EventKind]bool{}
	for _, evt := range collected {
		kinds[evt.Kind] = true
	}
	for _, want := range []launcherevents.EventKind{
		launcherevents.KindTurnStarted,
		launcherevents.KindAgentMessage,
		launcherevents.KindToolCall,
		launcherevents.KindCommandExec,
		launcherevents.KindToolResult,
		launcherevents.KindCommandOutput,
		launcherevents.KindTokenUsage,
		launcherevents.KindTurnCompleted,
	} {
		if !kinds[want] {
			t.Fatalf("missing event kind %s in %v", want, kinds)
		}
	}
}

func installFakeClaude(t *testing.T, lines []string) func() {
	t.Helper()

	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"cat <<'EOF'\n" +
		strings.Join(lines, "\n") + "\nEOF\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	return func() {
		_ = os.Setenv("PATH", oldPath)
	}
}
