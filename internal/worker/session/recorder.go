package session

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// Recorder centralizes worker-path session summary capture and structured event writes.
type Recorder struct {
	store       *store.Store
	auditor     *audit.Auditor
	sessionsDir string
	mu          sync.Mutex
	health      map[string]*SessionHealth
}

type SessionHealth struct {
	SessionID string        `json:"session_id"`
	Degraded  bool          `json:"degraded"`
	Issues    []HealthIssue `json:"issues,omitempty"`
}

type HealthIssue struct {
	Scope     string    `json:"scope"`
	Operation string    `json:"operation"`
	Message   string    `json:"message"`
	At        time.Time `json:"at"`
}

func NewRecorder(st *store.Store, sessionsDir string) *Recorder {
	var auditor *audit.Auditor
	if st != nil {
		auditor = audit.NewAuditor(st, sessionsDir)
	}
	return &Recorder{
		store:       st,
		auditor:     auditor,
		sessionsDir: sessionsDir,
	}
}

func (r *Recorder) Capture(sessionID, taskID, repo string, issueNum int, agentName string, result *runtimepkg.Result) error {
	if r == nil || r.auditor == nil {
		return nil
	}
	err := r.auditor.Capture(sessionID, taskID, repo, issueNum, agentName, result)
	if err != nil {
		r.noteIssue(sessionID, "audit", "capture", err)
	}
	r.applyResultHealth(sessionID, result)
	return err
}

func (r *Recorder) RecordLabelValidation(repo string, issueNum int, payload audit.LabelValidationPayload) error {
	return r.RecordLabelValidationSession("", repo, issueNum, payload)
}

func (r *Recorder) RecordLabelValidationSession(sessionID, repo string, issueNum int, payload audit.LabelValidationPayload) error {
	return r.RecordEventSession(sessionID, string(audit.EventKindLabelValidation), repo, issueNum, payload)
}

func (r *Recorder) RecordEvent(eventType, repo string, issueNum int, payload any) error {
	return r.RecordEventSession("", eventType, repo, issueNum, payload)
}

func (r *Recorder) RecordEventSession(sessionID, eventType, repo string, issueNum int, payload any) error {
	err := RecordEvent(r.store, eventType, repo, issueNum, payload)
	if err != nil {
		r.noteIssue(sessionID, "eventlog", eventType, err)
	}
	return err
}

func RecordEvent(st *store.Store, eventType, repo string, issueNum int, payload any) error {
	if st == nil {
		return nil
	}
	var payloadStr string
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("session recorder: marshal payload: %w", err)
		}
		payloadStr = string(data)
	}
	_, err := st.InsertEvent(store.Event{Type: eventType, Repo: repo, IssueNum: issueNum, Payload: payloadStr})
	if err != nil {
		return fmt.Errorf("session recorder: insert event: %w", err)
	}
	return nil
}

// Log implements reporter.EventRecorder for paths that expect fire-and-forget event logging.
func (r *Recorder) Log(eventType, repo string, issueNum int, payload interface{}) {
	if err := r.LogSession("", eventType, repo, issueNum, payload); err != nil {
		log.Printf("[session] failed to record %s for %s#%d: %v", eventType, repo, issueNum, err)
	}
}

func (r *Recorder) LogSession(sessionID, eventType, repo string, issueNum int, payload interface{}) error {
	return r.RecordEventSession(sessionID, eventType, repo, issueNum, payload)
}

func (r *Recorder) noteIssue(sessionID, scope, operation string, err error) {
	if r == nil || sessionID == "" || err == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.health == nil {
		r.health = make(map[string]*SessionHealth)
	}
	entry := r.health[sessionID]
	if entry == nil {
		entry = &SessionHealth{SessionID: sessionID}
		r.health[sessionID] = entry
	}
	entry.Degraded = true
	entry.Issues = append(entry.Issues, HealthIssue{
		Scope:     scope,
		Operation: operation,
		Message:   err.Error(),
		At:        time.Now().UTC(),
	})
	if writeErr := r.writeHealthLocked(sessionID, entry); writeErr != nil {
		log.Printf("[session] failed to persist health for %s: %v", sessionID, writeErr)
	}
}

func (r *Recorder) applyResultHealth(sessionID string, result *runtimepkg.Result) {
	if r == nil || sessionID == "" || result == nil {
		return
	}

	r.mu.Lock()
	entry := r.health[sessionID]
	var snapshot *SessionHealth
	if entry != nil {
		data := append([]HealthIssue(nil), entry.Issues...)
		snapshot = &SessionHealth{
			SessionID: entry.SessionID,
			Degraded:  entry.Degraded,
			Issues:    data,
		}
	}
	r.mu.Unlock()

	if snapshot == nil || !snapshot.Degraded {
		return
	}
	if result.Meta == nil {
		result.Meta = make(map[string]string)
	}
	result.Meta[runtimepkg.MetaStorageDegraded] = "true"
	if data, err := json.Marshal(snapshot.Issues); err == nil {
		result.Meta[runtimepkg.MetaStorageIssues] = string(data)
	}
}

func (r *Recorder) writeHealthLocked(sessionID string, health *SessionHealth) error {
	if r.sessionsDir == "" || sessionID == "" || health == nil {
		return nil
	}
	dir := filepath.Join(r.sessionsDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session recorder: create health dir: %w", err)
	}
	data, err := json.MarshalIndent(health, "", "  ")
	if err != nil {
		return fmt.Errorf("session recorder: marshal health: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, "health.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("session recorder: write health: %w", err)
	}
	return nil
}
