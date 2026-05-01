package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// SessionAnnouncer publishes a freshly-created session_id → worker_id
// route to the coordinator. Implementations are typically a thin
// adapter around workerclient.AnnounceSession; SessionManager calls it
// best-effort with a short timeout so the agent run isn't blocked on
// transient HTTP errors. Routes are also re-seeded on the next Register
// (Phase 4: bulk announce), so a single announce miss self-heals.
type SessionAnnouncer interface {
	AnnounceSession(ctx context.Context, sessionID, repo string, issueNum int) error
}

// announceTimeout caps the synchronous announce call. Long enough to
// tolerate a slow loopback HTTP round-trip in `serve` mode, short
// enough to keep agent boot snappy when the coordinator is wedged.
const announceTimeout = 3 * time.Second

type SessionManager struct {
	baseDir   string
	store     *store.Store
	announcer SessionAnnouncer
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

// SetAnnouncer wires the announcer used by Create after the store row is
// written. nil disables announcing — used by tests and by callers that
// share a DB with the coordinator (in which case Create's UpsertSessionRoute
// already covers the index).
func (m *SessionManager) SetAnnouncer(a SessionAnnouncer) {
	if m == nil {
		return
	}
	m.announcer = a
}

func (m *SessionManager) Create(input SessionCreateInput) (*ManagedSession, error) {
	if input.SessionID == "" {
		return nil, fmt.Errorf("runtime: session manager: missing session id")
	}
	dir := filepath.Join(m.baseDir, input.SessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("runtime: session manager: create dir: %w", err)
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
		// Local index: in `serve` mode the coordinator and worker share
		// this DB, so writing session_routes here lets the resolver find
		// the owning worker even before AnnounceSession completes (and
		// without the loopback HTTP round-trip). In split topology the
		// worker DB carries a harmless duplicate row; coordinator gets
		// its copy via the announce RPC below.
		if err := m.store.UpsertSessionRoute(store.SessionRoute{
			SessionID: record.SessionID,
			WorkerID:  record.WorkerID,
			Repo:      record.Repo,
			IssueNum:  record.IssueNum,
		}); err != nil {
			log.Printf("[session] upsert local route for %s: %v", record.SessionID, err)
		}
	}
	if m.announcer != nil && record.WorkerID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), announceTimeout)
		err := m.announcer.AnnounceSession(ctx, record.SessionID, record.Repo, record.IssueNum)
		cancel()
		if err != nil {
			// Best-effort: a missed announce self-heals on the next
			// Register (workers re-send their open sessions there).
			// Surface as a log warning so the operator can correlate
			// transient coordinator outages with delayed UI routing.
			log.Printf("[session] announce %s to coordinator: %v", record.SessionID, err)
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
			_, err := h.manager.store.CreateSession(h.record)
			return err
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
			return fmt.Errorf("runtime: session manager: open %s: %w", filepath.Base(path), err)
		}
		*target = file
	}
	if _, err := (*target).Write(data); err != nil {
		return fmt.Errorf("runtime: session manager: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (h *ManagedSession) flushMetadataLocked() error {
	data, err := json.MarshalIndent(h.metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime: session manager: marshal metadata: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(h.record.MetadataPath, data, 0o644); err != nil {
		return fmt.Errorf("runtime: session manager: write metadata: %w", err)
	}
	return nil
}
