// Package workflow tracks per-issue workflow progress and transition history.
package workflow

import (
	"errors"
	"fmt"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// StateTransition represents one transition in a workflow instance.
type StateTransition struct {
	From         string
	To           string
	Timestamp    time.Time
	TriggerAgent string
}

// WorkflowInstance tracks the current state and transition history of one issue.
type WorkflowInstance struct {
	ID           string
	WorkflowName string
	Repo         string
	IssueNum     int
	CurrentState string
	History      []StateTransition
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ErrWorkflowInstanceNotFound is returned when requested instance rows do not exist.
var ErrWorkflowInstanceNotFound = errors.New("workflow instance not found")

// Repository is the narrow persistence surface required by Manager. It is
// satisfied by *store.Store and is defined here so workflow.Manager does not
// have to reach through to a raw *sql.DB.
type Repository interface {
	CreateWorkflowInstanceIfMissing(id, workflowName, repo string, issueNum int, currentState string) error
	AdvanceWorkflowInstance(id, fromState, toState, triggerAgent string, at time.Time) error
	QueryWorkflowInstancesByRepoIssue(repo string, issueNum int) ([]store.WorkflowInstanceRow, error)
	GetWorkflowInstanceByID(id string) (*store.WorkflowInstanceRow, error)
	QueryWorkflowTransitions(instanceID string) ([]store.WorkflowTransitionRow, error)
}

// Manager provides CRUD operations for workflow instances and transitions.
type Manager struct {
	repo Repository
}

// NewManager creates a workflow manager bound to the given store-backed
// repository. Passing nil is permitted but every subsequent call will return
// "workflow manager not initialized".
func NewManager(repo Repository) *Manager {
	return &Manager{repo: repo}
}

// CreateIfMissing creates a WorkflowInstance if one does not exist.
// Existing state is preserved if the record already exists.
func (m *Manager) CreateIfMissing(
	repo string, issueNum int, workflowName, currentState string,
) error {
	if m == nil || m.repo == nil {
		return errors.New("workflow manager not initialized")
	}
	id := makeInstanceID(repo, issueNum, workflowName)
	if err := m.repo.CreateWorkflowInstanceIfMissing(id, workflowName, repo, issueNum, currentState); err != nil {
		return fmt.Errorf("create workflow instance: %w", err)
	}
	return nil
}

// Advance writes a new transition and updates CurrentState atomically.
func (m *Manager) Advance(repo string, issueNum int, workflowName, fromState, toState, triggerAgent string) error {
	if m == nil || m.repo == nil {
		return errors.New("workflow manager not initialized")
	}
	id := makeInstanceID(repo, issueNum, workflowName)
	err := m.repo.AdvanceWorkflowInstance(id, fromState, toState, triggerAgent, time.Now().UTC())
	if errors.Is(err, store.ErrWorkflowInstanceNotFound) {
		return ErrWorkflowInstanceNotFound
	}
	if err != nil {
		return fmt.Errorf("advance workflow instance: %w", err)
	}
	return nil
}

// QueryByRepoIssue returns all workflow instances for one issue with history.
func (m *Manager) QueryByRepoIssue(repo string, issueNum int) ([]WorkflowInstance, error) {
	if m == nil || m.repo == nil {
		return nil, errors.New("workflow manager not initialized")
	}
	rows, err := m.repo.QueryWorkflowInstancesByRepoIssue(repo, issueNum)
	if err != nil {
		return nil, fmt.Errorf("query workflow instances: %w", err)
	}
	out := make([]WorkflowInstance, 0, len(rows))
	for _, r := range rows {
		inst := instanceFromRow(r)
		inst.History, err = m.loadHistory(inst.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, nil
}

// GetByID loads one WorkflowInstance and its transitions by ID.
func (m *Manager) GetByID(id string) (*WorkflowInstance, error) {
	if m == nil || m.repo == nil {
		return nil, errors.New("workflow manager not initialized")
	}
	row, err := m.repo.GetWorkflowInstanceByID(id)
	if errors.Is(err, store.ErrWorkflowInstanceNotFound) {
		return nil, ErrWorkflowInstanceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query workflow instance by id: %w", err)
	}
	inst := instanceFromRow(*row)
	inst.History, err = m.loadHistory(inst.ID)
	if err != nil {
		return nil, err
	}
	return &inst, nil
}

func (m *Manager) loadHistory(instanceID string) ([]StateTransition, error) {
	rows, err := m.repo.QueryWorkflowTransitions(instanceID)
	if err != nil {
		return nil, fmt.Errorf("query workflow transitions: %w", err)
	}
	out := make([]StateTransition, 0, len(rows))
	for _, r := range rows {
		ts, _ := store.ParseTimestamp(r.CreatedAt, "workflow_transition.created_at")
		out = append(out, StateTransition{
			From:         r.FromState,
			To:           r.ToState,
			Timestamp:    ts,
			TriggerAgent: r.TriggerAgent,
		})
	}
	return out, nil
}

func instanceFromRow(r store.WorkflowInstanceRow) WorkflowInstance {
	inst := WorkflowInstance{
		ID:           r.ID,
		WorkflowName: r.WorkflowName,
		Repo:         r.Repo,
		IssueNum:     r.IssueNum,
		CurrentState: r.CurrentState,
	}
	inst.CreatedAt, _ = store.ParseTimestamp(r.CreatedAt, "workflow.created_at")
	inst.UpdatedAt, _ = store.ParseTimestamp(r.UpdatedAt, "workflow.updated_at")
	return inst
}

func makeInstanceID(repo string, issueNum int, workflowName string) string {
	return fmt.Sprintf("%s#%d#%s", repo, issueNum, workflowName)
}
