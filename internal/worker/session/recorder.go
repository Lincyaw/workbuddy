package session

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/Lincyaw/workbuddy/internal/audit"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// Recorder centralizes worker-path session summary capture and structured event writes.
type Recorder struct {
	store   *store.Store
	auditor *audit.Auditor
}

func NewRecorder(st *store.Store, sessionsDir string) *Recorder {
	return &Recorder{store: st, auditor: audit.NewAuditor(st, sessionsDir)}
}

func (r *Recorder) Capture(sessionID, taskID, repo string, issueNum int, agentName string, result *runtimepkg.Result) error {
	if r == nil || r.auditor == nil {
		return nil
	}
	return r.auditor.Capture(sessionID, taskID, repo, issueNum, agentName, result)
}

func (r *Recorder) RecordLabelValidation(repo string, issueNum int, payload audit.LabelValidationPayload) error {
	return r.RecordEvent(string(audit.EventKindLabelValidation), repo, issueNum, payload)
}

func (r *Recorder) RecordEvent(eventType, repo string, issueNum int, payload any) error {
	return RecordEvent(r.store, eventType, repo, issueNum, payload)
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
	if err := r.RecordEvent(eventType, repo, issueNum, payload); err != nil {
		log.Printf("[session] failed to record %s for %s#%d: %v", eventType, repo, issueNum, err)
	}
}
