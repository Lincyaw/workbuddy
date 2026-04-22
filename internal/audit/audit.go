// Package audit captures and queries agent session artifacts.
package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/launcher"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type EventKind string

const EventKindLabelValidation EventKind = "label.validation"

type LabelValidationPayload struct {
	Pre            []string `json:"pre"`
	Post           []string `json:"post"`
	ExitCode       int      `json:"exit_code"`
	Classification string   `json:"classification"`
}

// maxSummarySize is the threshold (1 MB) above which only a truncated summary
// is stored in SQLite; the full file is kept on disk.
const maxSummarySize = 1 << 20 // 1 MB

// Filter specifies optional query predicates.  Zero-value fields are ignored.
type Filter struct {
	SessionID string
	IssueNum  int
	AgentName string
}

// Auditor captures and queries agent session artifacts.
type Auditor struct {
	store       *store.Store
	sessionsDir string // e.g. ".workbuddy/sessions"
}

// NewAuditor creates an Auditor that archives raw session files under sessionsDir
// and persists summaries in the given store.
func NewAuditor(s *store.Store, sessionsDir string) *Auditor {
	return &Auditor{store: s, sessionsDir: sessionsDir}
}

// Capture reads the session artifact produced by an agent run, generates a
// human-readable summary, archives the raw file, and stores a record in SQLite.
//
// If result.SessionPath is empty or the file does not exist, a minimal record
// (stdout/stderr excerpt) is stored instead.
func (a *Auditor) Capture(sessionID, taskID, repo string, issueNum int, agentName string, result *launcher.Result) error {
	archiveDir := filepath.Join(a.sessionsDir, sessionID)
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return fmt.Errorf("audit: create archive dir: %w", err)
	}

	var summary string
	var rawPath string

	if result.SessionPath != "" {
		data, err := os.ReadFile(result.SessionPath)
		if err != nil {
			// File missing / unreadable — fall through to minimal record.
			summary = buildMinimalSummary(result)
		} else {
			// Archive the raw file.
			dst := filepath.Join(archiveDir, filepath.Base(result.SessionPath))
			if err := copyFile(result.SessionPath, dst); err != nil {
				return fmt.Errorf("audit: archive session file: %w", err)
			}
			rawPath = dst

			// Generate summary based on runtime.
			switch {
			case result != nil && result.SessionRef.Kind == "codex-thread":
				summary = summarizeCodex(result, string(data))
			case strings.Contains(strings.ToLower(agentName), "codex"):
				summary = summarizeCodex(result, string(data))
			default: // claude-code or unknown — try JSON parse
				summary = summarizeClaude(data)
			}

			// Truncate if over 1 MB.
			if len(summary) > maxSummarySize {
				summary = summary[:maxSummarySize] + "\n... [truncated, full session on disk]"
			}
		}
	} else {
		summary = buildMinimalSummary(result)
	}

	sess := store.AgentSession{
		SessionID: sessionID,
		TaskID:    taskID,
		Repo:      repo,
		IssueNum:  issueNum,
		AgentName: agentName,
		Summary:   summary,
		RawPath:   rawPath,
	}
	// If the session was pre-recorded at task start, update it; otherwise insert.
	existing, err := a.store.GetAgentSession(sessionID)
	if err != nil {
		return fmt.Errorf("audit: check session: %w", err)
	}
	if existing != nil {
		if err := a.store.UpdateAgentSession(sessionID, summary, rawPath); err != nil {
			return fmt.Errorf("audit: update session: %w", err)
		}
	} else {
		if _, err := a.store.InsertAgentSession(sess); err != nil {
			return fmt.Errorf("audit: insert session: %w", err)
		}
	}
	return nil
}

func (a *Auditor) RecordEvent(kind EventKind, repo string, issueNum int, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("audit: marshal %s payload: %w", kind, err)
	}
	if _, err := a.store.InsertEvent(store.Event{
		Type:     string(kind),
		Repo:     repo,
		IssueNum: issueNum,
		Payload:  string(data),
	}); err != nil {
		return fmt.Errorf("audit: insert %s event: %w", kind, err)
	}
	return nil
}

func (a *Auditor) RecordLabelValidation(repo string, issueNum int, payload LabelValidationPayload) error {
	return a.RecordEvent(EventKindLabelValidation, repo, issueNum, payload)
}

// Query returns sessions matching the given filter.
func (a *Auditor) Query(filter Filter) ([]store.AgentSession, error) {
	sessions, err := a.store.ListAgentSessions(store.SessionFilter{
		IssueNum:  filter.IssueNum,
		AgentName: filter.AgentName,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: query sessions: %w", err)
	}
	if filter.SessionID == "" {
		return sessions, nil
	}
	var out []store.AgentSession
	for _, session := range sessions {
		if session.SessionID == filter.SessionID {
			out = append(out, session)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Claude Code session parsing
// ---------------------------------------------------------------------------

// claudeMessage represents a single message in a Claude Code session JSON.
type claudeMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	StopReason string          `json:"stop_reason"`
}

// claudeContentBlock is one element in the content array.
type claudeContentBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Name  string `json:"name"`
	Input any    `json:"input"`
}

// claudeSession is the top-level structure of a Claude Code session file.
type claudeSession struct {
	Messages []claudeMessage        `json:"messages"`
	Usage    map[string]interface{} `json:"usage"`
	Metadata map[string]interface{} `json:"metadata"`
}

func summarizeClaude(data []byte) string {
	var sess claudeSession
	if err := json.Unmarshal(data, &sess); err != nil {
		// Not valid JSON — treat as plain text.
		return truncate(string(data), 2000)
	}

	var b strings.Builder
	b.WriteString("## Claude Code Session Summary\n\n")

	// Usage info.
	if sess.Usage != nil {
		b.WriteString("### Token Usage\n")
		for k, v := range sess.Usage {
			fmt.Fprintf(&b, "- %s: %v\n", k, v)
		}
		b.WriteString("\n")
	}

	// Count tool calls and collect names.
	toolCounts := map[string]int{}
	for _, msg := range sess.Messages {
		if msg.Role != "assistant" {
			continue
		}
		// Try to parse content as array of blocks.
		var blocks []claudeContentBlock
		if err := json.Unmarshal(msg.Content, &blocks); err == nil {
			for _, blk := range blocks {
				if blk.Type == "tool_use" && blk.Name != "" {
					toolCounts[blk.Name]++
				}
			}
		}
	}

	if len(toolCounts) > 0 {
		b.WriteString("### Tool Calls\n")
		for name, count := range toolCounts {
			fmt.Fprintf(&b, "- %s: %d\n", name, count)
		}
		b.WriteString("\n")
	}

	// Extract text snippets from assistant messages (first 500 chars total).
	var textParts []string
	charBudget := 500
	for _, msg := range sess.Messages {
		if msg.Role != "assistant" || charBudget <= 0 {
			continue
		}
		var blocks []claudeContentBlock
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			continue
		}
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != "" {
				t := truncate(blk.Text, charBudget)
				textParts = append(textParts, t)
				charBudget -= len(t)
			}
		}
	}
	if len(textParts) > 0 {
		b.WriteString("### Key Responses\n")
		for _, t := range textParts {
			fmt.Fprintf(&b, "> %s\n\n", t)
		}
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// Codex session parsing
// ---------------------------------------------------------------------------

func summarizeCodex(result *launcher.Result, data string) string {
	if summary, ok := summarizeCodexEvents(result, data); ok {
		return summary
	}

	var b strings.Builder
	b.WriteString("## Codex Session Summary\n\n")
	if result != nil && result.LastMessage != "" {
		b.WriteString("### Final Message\n")
		fmt.Fprintf(&b, "> %s\n\n", truncate(result.LastMessage, 500))
	}

	lines := strings.Split(data, "\n")
	var keyLines []string
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") ||
			strings.Contains(lower, "warning") ||
			strings.Contains(lower, "completed") ||
			strings.Contains(lower, "success") ||
			strings.Contains(lower, "failed") ||
			strings.Contains(lower, "result") ||
			strings.Contains(lower, "output") {
			keyLines = append(keyLines, strings.TrimSpace(line))
		}
	}

	if len(keyLines) > 0 {
		b.WriteString("### Key Lines\n")
		for _, l := range keyLines {
			if len(l) > 200 {
				l = l[:200] + "..."
			}
			fmt.Fprintf(&b, "- %s\n", l)
		}
	} else {
		b.WriteString("### Log Excerpt\n```\n")
		limit := 20
		if len(lines) < limit {
			limit = len(lines)
		}
		for _, l := range lines[:limit] {
			b.WriteString(l)
			b.WriteString("\n")
		}
		b.WriteString("```\n")
	}

	return b.String()
}

func summarizeCodexEvents(result *launcher.Result, data string) (string, bool) {
	lines := strings.Split(strings.TrimSpace(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return "", false
	}

	var first launcherevents.Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Kind == "" {
		return "", false
	}

	var b strings.Builder
	b.WriteString("## Codex Session Summary\n\n")
	if result != nil && result.LastMessage != "" {
		b.WriteString("### Final Message\n")
		fmt.Fprintf(&b, "> %s\n\n", truncate(result.LastMessage, 500))
	}

	eventCounts := map[launcherevents.EventKind]int{}
	var lastUsage *launcherevents.TokenUsagePayload
	var commands []string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt launcherevents.Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		eventCounts[evt.Kind]++
		switch evt.Kind {
		case launcherevents.KindCommandExec:
			var payload launcherevents.CommandExecPayload
			if err := json.Unmarshal(evt.Payload, &payload); err == nil && len(payload.Cmd) > 0 {
				commands = append(commands, strings.Join(payload.Cmd, " "))
			}
		case launcherevents.KindTokenUsage:
			var payload launcherevents.TokenUsagePayload
			if err := json.Unmarshal(evt.Payload, &payload); err == nil {
				lastUsage = &payload
			}
		}
	}

	b.WriteString("### Event Counts\n")
	for _, kind := range []launcherevents.EventKind{
		launcherevents.KindTurnStarted,
		launcherevents.KindAgentMessage,
		launcherevents.KindCommandExec,
		launcherevents.KindCommandOutput,
		launcherevents.KindToolCall,
		launcherevents.KindToolResult,
		launcherevents.KindFileChange,
		launcherevents.KindTokenUsage,
		launcherevents.KindTurnCompleted,
		launcherevents.KindError,
		launcherevents.KindLog,
	} {
		if eventCounts[kind] == 0 {
			continue
		}
		fmt.Fprintf(&b, "- %s: %d\n", kind, eventCounts[kind])
	}
	b.WriteString("\n")

	if len(commands) > 0 {
		b.WriteString("### Commands\n")
		for _, command := range commands {
			fmt.Fprintf(&b, "- %s\n", truncate(command, 200))
		}
		b.WriteString("\n")
	}

	if lastUsage != nil {
		b.WriteString("### Token Usage\n")
		fmt.Fprintf(&b, "- input: %d\n- output: %d\n- cached: %d\n- total: %d\n", lastUsage.Input, lastUsage.Output, lastUsage.Cached, lastUsage.Total)
	}

	return b.String(), true
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func buildMinimalSummary(result *launcher.Result) string {
	var b strings.Builder
	b.WriteString("## Session Summary (no session file)\n\n")
	fmt.Fprintf(&b, "- Exit code: %d\n", result.ExitCode)
	fmt.Fprintf(&b, "- Duration: %s\n", result.Duration)
	if result.LastMessage != "" {
		b.WriteString("\n### Final Message\n```\n")
		b.WriteString(truncate(result.LastMessage, 500))
		b.WriteString("\n```\n")
	}
	if result.Stdout != "" {
		b.WriteString("\n### Stdout (excerpt)\n```\n")
		b.WriteString(truncate(result.Stdout, 500))
		b.WriteString("\n```\n")
	}
	if result.Stderr != "" {
		b.WriteString("\n### Stderr (excerpt)\n```\n")
		b.WriteString(truncate(result.Stderr, 500))
		b.WriteString("\n```\n")
	}
	return b.String()
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func copyFile(src, dst string) error {
	if src == dst {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
