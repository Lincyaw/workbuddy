package launcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type SessionManager struct {
	baseDir string
	store   *store.Store
}

type SessionCreateInput struct {
	SessionID string
	TaskID    string
	Repo      string
	IssueNum  int
	AgentName string
	Runtime   string
	WorkerID  string
	Attempt   int
}

type sessionMetadata struct {
	SessionID     string    `json:"session_id"`
	TaskID        string    `json:"task_id,omitempty"`
	Repo          string    `json:"repo"`
	IssueNum      int       `json:"issue_num"`
	AgentName     string    `json:"agent_name"`
	Runtime       string    `json:"runtime,omitempty"`
	WorkerID      string    `json:"worker_id,omitempty"`
	Attempt       int       `json:"attempt"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	ClosedAt      time.Time `json:"closed_at,omitempty"`
	StdoutPath    string    `json:"stdout_path"`
	StderrPath    string    `json:"stderr_path"`
	ToolCallsPath string    `json:"tool_calls_path"`
	EventsPath    string    `json:"events_path"`
}

type ManagedSession struct {
	manager       *SessionManager
	record        store.SessionRecord
	eventsPath    string
	metadata      sessionMetadata
	mu            sync.Mutex
	stdoutFile    *os.File
	stderrFile    *os.File
	toolCallsFile *os.File
	eventsFile    *os.File
}

func NewSessionManager(baseDir string, st *store.Store) *SessionManager {
	return &SessionManager{baseDir: baseDir, store: st}
}

func (m *SessionManager) Create(input SessionCreateInput) (*ManagedSession, error) {
	if input.SessionID == "" {
		return nil, fmt.Errorf("launcher: session manager: missing session id")
	}
	dir := filepath.Join(m.baseDir, input.SessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("launcher: session manager: create dir: %w", err)
	}
	record := store.SessionRecord{
		SessionID:     input.SessionID,
		TaskID:        input.TaskID,
		Repo:          input.Repo,
		IssueNum:      input.IssueNum,
		AgentName:     input.AgentName,
		Runtime:       input.Runtime,
		WorkerID:      input.WorkerID,
		Attempt:       input.Attempt,
		Status:        store.TaskStatusRunning,
		Dir:           dir,
		StdoutPath:    filepath.Join(dir, "stdout"),
		StderrPath:    filepath.Join(dir, "stderr"),
		ToolCallsPath: filepath.Join(dir, "tool-calls.jsonl"),
		MetadataPath:  filepath.Join(dir, "metadata.json"),
		CreatedAt:     time.Now().UTC(),
	}
	handle := &ManagedSession{
		manager:    m,
		record:     record,
		eventsPath: filepath.Join(dir, "events-v1.jsonl"),
		metadata: sessionMetadata{
			SessionID:     record.SessionID,
			TaskID:        record.TaskID,
			Repo:          record.Repo,
			IssueNum:      record.IssueNum,
			AgentName:     record.AgentName,
			Runtime:       record.Runtime,
			WorkerID:      record.WorkerID,
			Attempt:       record.Attempt,
			Status:        record.Status,
			CreatedAt:     record.CreatedAt,
			StdoutPath:    record.StdoutPath,
			StderrPath:    record.StderrPath,
			ToolCallsPath: record.ToolCallsPath,
			EventsPath:    filepath.Join(dir, "events-v1.jsonl"),
		},
	}
	if err := handle.flushMetadataLocked(); err != nil {
		return nil, err
	}
	if m.store != nil {
		if _, err := m.store.CreateSession(record); err != nil {
			return nil, err
		}
	}
	return handle, nil
}

func (m *SessionManager) Get(sessionID string) (*store.SessionRecord, error) {
	if m.store == nil {
		return nil, nil
	}
	return m.store.GetSession(sessionID)
}

func (m *SessionManager) List(filter store.SessionFilter) ([]store.SessionRecord, error) {
	if m.store == nil {
		return nil, nil
	}
	return m.store.ListSessions(filter)
}

func (h *ManagedSession) Dir() string { return h.record.Dir }

func (h *ManagedSession) StdoutPath() string { return h.record.StdoutPath }

func (h *ManagedSession) StderrPath() string { return h.record.StderrPath }

func (h *ManagedSession) ToolCallsPath() string { return h.record.ToolCallsPath }

func (h *ManagedSession) EventsPath() string { return h.eventsPath }

func (h *ManagedSession) MetadataPath() string { return h.record.MetadataPath }

func (h *ManagedSession) WriteStdout(data []byte) error {
	return h.writeFile(&h.stdoutFile, h.record.StdoutPath, data)
}

func (h *ManagedSession) WriteStderr(data []byte) error {
	return h.writeFile(&h.stderrFile, h.record.StderrPath, data)
}

func (h *ManagedSession) WriteToolCall(data []byte) error {
	return h.writeFile(&h.toolCallsFile, h.record.ToolCallsPath, data)
}

func (h *ManagedSession) WriteEvent(data []byte) error {
	return h.writeFile(&h.eventsFile, h.eventsPath, data)
}

func (h *ManagedSession) Close(status string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if status == "" {
		status = store.TaskStatusCompleted
	}
	for _, file := range []*os.File{h.stdoutFile, h.stderrFile, h.toolCallsFile, h.eventsFile} {
		if file != nil {
			_ = file.Close()
		}
	}
	h.metadata.Status = status
	h.metadata.ClosedAt = time.Now().UTC()
	h.record.Status = status
	h.record.ClosedAt = h.metadata.ClosedAt
	if err := h.flushMetadataLocked(); err != nil {
		return err
	}
	if h.manager != nil && h.manager.store != nil {
		record, err := h.manager.store.GetSession(h.record.SessionID)
		if err != nil {
			return err
		}
		if record == nil {
			return h.manager.store.UpdateSession(h.record)
		}
		record.Status = h.record.Status
		record.ClosedAt = h.record.ClosedAt
		return h.manager.store.UpdateSession(*record)
	}
	return nil
}

func (h *ManagedSession) writeFile(target **os.File, path string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if *target == nil {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("launcher: session manager: open %s: %w", filepath.Base(path), err)
		}
		*target = file
	}
	if _, err := (*target).Write(data); err != nil {
		return fmt.Errorf("launcher: session manager: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (h *ManagedSession) flushMetadataLocked() error {
	data, err := json.MarshalIndent(h.metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("launcher: session manager: marshal metadata: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(h.record.MetadataPath, data, 0o644); err != nil {
		return fmt.Errorf("launcher: session manager: write metadata: %w", err)
	}
	return nil
}

func persistToolCallEvent(handle *ManagedSession, runtime string, evt launcherevents.Event) error {
	if handle == nil {
		return nil
	}
	switch evt.Kind {
	case launcherevents.KindToolCall,
		launcherevents.KindToolResult,
		launcherevents.KindCommandExec,
		launcherevents.KindCommandOutput,
		launcherevents.KindFileChange:
	default:
		return nil
	}
	record := map[string]any{
		"runtime": runtime,
		"kind":    evt.Kind,
		"seq":     evt.Seq,
		"turn_id": evt.TurnID,
		"payload": json.RawMessage(evt.Payload),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("launcher: session manager: marshal tool call: %w", err)
	}
	return handle.WriteToolCall(append(data, '\n'))
}
