package codex

import (
	"encoding/json"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/agent"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

// mapNotification converts a Codex app-server notification into one or more
// normalized agent events whose payloads match the launcher's Event Schema v1
// shapes.
func mapNotification(method string, params, raw json.RawMessage) []agent.Event {
	if len(raw) == 0 {
		raw = params
	}
	switch method {
	case "turn/started":
		turnID := turnIDFromTurn(params)
		if turnID == "" {
			turnID = extractTurnID(params)
		}
		return []agent.Event{newEvent("turn.started", turnID, launcherevents.TurnStartedPayload{TurnID: turnID}, raw)}
	case "turn/completed":
		turnID, status := turnCompletionInfo(params)
		return []agent.Event{
			newEvent("turn.completed", turnID, launcherevents.TurnCompletedPayload{TurnID: turnID, Status: status}, raw),
			newEvent("task.complete", turnID, launcherevents.TaskCompletePayload{TurnID: turnID, Status: status}, raw),
		}
	case "item/agentMessage/delta":
		var payload struct {
			Delta  string `json:"delta"`
			TurnID string `json:"turnId"`
		}
		if err := json.Unmarshal(params, &payload); err != nil || strings.TrimSpace(payload.Delta) == "" {
			return nil
		}
		return []agent.Event{newEvent("agent.message", payload.TurnID, launcherevents.AgentMessagePayload{Text: payload.Delta, Delta: true, Final: false}, raw)}
	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		var payload struct {
			Delta  string `json:"delta"`
			TurnID string `json:"turnId"`
		}
		if err := json.Unmarshal(params, &payload); err != nil || strings.TrimSpace(payload.Delta) == "" {
			return nil
		}
		return []agent.Event{newEvent("reasoning", payload.TurnID, launcherevents.ReasoningPayload{Text: payload.Delta, Delta: true}, raw)}
	case "item/commandExecution/outputDelta":
		var payload struct {
			Delta  string `json:"delta"`
			ItemID string `json:"itemId"`
			TurnID string `json:"turnId"`
		}
		if err := json.Unmarshal(params, &payload); err != nil || payload.Delta == "" {
			return nil
		}
		return []agent.Event{newEvent("command.output", payload.TurnID, launcherevents.CommandOutputPayload{CallID: payload.ItemID, Stream: "stdout", Data: payload.Delta}, raw)}
	case "thread/tokenUsage/updated":
		var payload struct {
			TurnID     string `json:"turnId"`
			TokenUsage struct {
				Total struct {
					InputTokens       int `json:"inputTokens"`
					OutputTokens      int `json:"outputTokens"`
					CachedInputTokens int `json:"cachedInputTokens"`
					TotalTokens       int `json:"totalTokens"`
				} `json:"total"`
			} `json:"tokenUsage"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return nil
		}
		usage := launcherevents.TokenUsagePayload{
			Input:  payload.TokenUsage.Total.InputTokens,
			Output: payload.TokenUsage.Total.OutputTokens,
			Cached: payload.TokenUsage.Total.CachedInputTokens,
			Total:  payload.TokenUsage.Total.TotalTokens,
		}
		return []agent.Event{newEvent("token.usage", payload.TurnID, usage, raw)}
	case "error":
		return []agent.Event{mapErrorNotification(params, raw)}
	case "item/started":
		return mapThreadItemEvent(params, raw, false)
	case "item/completed":
		return mapThreadItemEvent(params, raw, true)
	default:
		return nil
	}
}

func mapThreadItemEvent(params, raw json.RawMessage, completed bool) []agent.Event {
	var payload struct {
		Item   map[string]json.RawMessage `json:"item"`
		TurnID string                     `json:"turnId"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return nil
	}

	itemType := rawString(payload.Item, "type")
	switch itemType {
	case "agentMessage":
		text := strings.TrimSpace(rawString(payload.Item, "text"))
		if text == "" {
			return nil
		}
		phase := rawString(payload.Item, "phase")
		final := completed && phase == "final_answer"
		if completed && phase == "" {
			final = true
		}
		return []agent.Event{newEvent("agent.message", payload.TurnID, launcherevents.AgentMessagePayload{Text: text, Delta: false, Final: final}, raw)}
	case "reasoning":
		text := strings.TrimSpace(strings.Join(append(rawStringSlice(payload.Item, "summary"), rawStringSlice(payload.Item, "content")...), "\n"))
		if text == "" {
			return nil
		}
		return []agent.Event{newEvent("reasoning", payload.TurnID, launcherevents.ReasoningPayload{Text: text, Delta: false}, raw)}
	case "commandExecution":
		if !completed {
			return []agent.Event{newEvent("command.exec", payload.TurnID, launcherevents.CommandExecPayload{Cmd: commandArgs(payload.Item), CWD: rawString(payload.Item, "cwd"), CallID: rawString(payload.Item, "id")}, raw)}
		}
		var out []agent.Event
		if output := rawString(payload.Item, "aggregatedOutput"); output != "" {
			out = append(out, newEvent("command.output", payload.TurnID, launcherevents.CommandOutputPayload{CallID: rawString(payload.Item, "id"), Stream: "stdout", Data: output}, raw))
		}
		out = append(out, newEvent("tool.result", payload.TurnID, launcherevents.ToolResultPayload{CallID: rawString(payload.Item, "id"), OK: rawString(payload.Item, "status") == "completed", Result: cloneRaw(payload.Item["aggregatedOutput"])}, raw))
		return out
	case "mcpToolCall":
		if !completed {
			return []agent.Event{newEvent("tool.call", payload.TurnID, launcherevents.ToolCallPayload{Name: rawString(payload.Item, "tool"), CallID: rawString(payload.Item, "id"), Args: cloneRaw(payload.Item["arguments"])}, raw)}
		}
		result := cloneRaw(payload.Item["result"])
		if len(result) == 0 {
			result = cloneRaw(payload.Item["error"])
		}
		return []agent.Event{newEvent("tool.result", payload.TurnID, launcherevents.ToolResultPayload{CallID: rawString(payload.Item, "id"), OK: rawString(payload.Item, "status") == "completed", Result: result}, raw)}
	case "dynamicToolCall":
		if !completed {
			return []agent.Event{newEvent("tool.call", payload.TurnID, launcherevents.ToolCallPayload{Name: rawString(payload.Item, "tool"), CallID: rawString(payload.Item, "id"), Args: cloneRaw(payload.Item["arguments"])}, raw)}
		}
		return []agent.Event{newEvent("tool.result", payload.TurnID, launcherevents.ToolResultPayload{CallID: rawString(payload.Item, "id"), OK: rawBool(payload.Item, "success"), Result: cloneRaw(payload.Item["contentItems"])}, raw)}
	case "fileChange":
		if !completed {
			return nil
		}
		changes := rawPatchChanges(payload.Item)
		out := make([]agent.Event, 0, len(changes))
		for _, change := range changes {
			out = append(out, newEvent("file.change", payload.TurnID, change, raw))
		}
		return out
	default:
		return nil
	}
}

func mapErrorNotification(params, raw json.RawMessage) agent.Event {
	var payload struct {
		TurnID    string `json:"turnId"`
		WillRetry bool   `json:"willRetry"`
		Error     struct {
			Message        string          `json:"message"`
			CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
		} `json:"error"`
	}
	_ = json.Unmarshal(params, &payload)
	code := codexErrorCode(payload.Error.CodexErrorInfo)
	if code == "" {
		code = "codex_error"
	}
	return newEvent("error", payload.TurnID, launcherevents.ErrorPayload{Code: code, Message: payload.Error.Message, Recoverable: payload.WillRetry}, raw)
}

func newEvent(kind, turnID string, payload any, raw json.RawMessage) agent.Event {
	return agent.Event{
		Kind:   kind,
		TurnID: turnID,
		Body:   launcherevents.MustPayload(payload),
		Raw:    cloneRaw(raw),
	}
}

func notificationKind(method string, params json.RawMessage) string {
	events := mapNotification(method, params, nil)
	if len(events) == 0 {
		return ""
	}
	return events[0].Kind
}

func extractThreadID(params json.RawMessage) string {
	var direct struct {
		ThreadID       string `json:"threadId"`
		ThreadID2      string `json:"thread_id"`
		ConversationID string `json:"conversationId"`
		Thread         struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(params, &direct); err != nil {
		return ""
	}
	switch {
	case direct.ThreadID != "":
		return direct.ThreadID
	case direct.ThreadID2 != "":
		return direct.ThreadID2
	case direct.ConversationID != "":
		return direct.ConversationID
	default:
		return direct.Thread.ID
	}
}

func extractTurnID(params json.RawMessage) string {
	var direct struct {
		TurnID  string `json:"turnId"`
		TurnID2 string `json:"turn_id"`
	}
	if err := json.Unmarshal(params, &direct); err != nil {
		return ""
	}
	if direct.TurnID != "" {
		return direct.TurnID
	}
	return direct.TurnID2
}

func turnIDFromTurn(params json.RawMessage) string {
	var payload struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return ""
	}
	return payload.Turn.ID
}

func turnCompletionInfo(params json.RawMessage) (turnID, status string) {
	var payload struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return "", "completed"
	}
	if payload.Turn.Status == "" {
		payload.Turn.Status = "completed"
	}
	return payload.Turn.ID, payload.Turn.Status
}

func codexErrorCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		return simple
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for key := range obj {
		return key
	}
	return ""
}

func commandArgs(raw map[string]json.RawMessage) []string {
	if values := rawStringSlice(raw, "command"); len(values) > 0 {
		return values
	}
	if cmd := strings.TrimSpace(rawString(raw, "command")); cmd != "" {
		return []string{cmd}
	}
	return nil
}

func rawString(raw map[string]json.RawMessage, keys ...string) string {
	if raw == nil {
		return ""
	}
	for _, key := range keys {
		var s string
		if err := json.Unmarshal(raw[key], &s); err == nil && s != "" {
			return s
		}
	}
	return ""
}

func rawStringSlice(raw map[string]json.RawMessage, key string) []string {
	if raw == nil {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw[key], &values); err == nil && len(values) > 0 {
		return values
	}
	return nil
}

func rawBool(raw map[string]json.RawMessage, key string) bool {
	if raw == nil {
		return false
	}
	var v bool
	if err := json.Unmarshal(raw[key], &v); err == nil {
		return v
	}
	return false
}

func rawObject(raw map[string]json.RawMessage, key string) map[string]json.RawMessage {
	if raw == nil {
		return nil
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw[key], &out); err == nil {
		return out
	}
	return nil
}

func rawPatchChanges(raw map[string]json.RawMessage) []launcherevents.FileChangePayload {
	changes := rawArrayOfObjects(raw, "changes")
	out := make([]launcherevents.FileChangePayload, 0, len(changes))
	for _, change := range changes {
		kind := rawString(rawObject(change, "kind"), "type")
		path := rawString(change, "path")
		if path == "" || kind == "" {
			continue
		}
		out = append(out, launcherevents.FileChangePayload{
			Path:       path,
			ChangeKind: kind,
			Diff:       rawString(change, "diff"),
		})
	}
	return out
}

func rawArrayOfObjects(raw map[string]json.RawMessage, key string) []map[string]json.RawMessage {
	if raw == nil {
		return nil
	}
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(raw[key], &out); err == nil {
		return out
	}
	return nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
