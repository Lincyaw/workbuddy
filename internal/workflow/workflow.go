// Package workflow tracks per-issue workflow progress and transition history.
package workflow

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
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

var timeLayouts = []string{
	"2006-01-02 15:04:05",
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05Z",
}

// Manager provides CRUD operations for workflow instances and transitions.
type Manager struct {
	db *sql.DB
}

// NewManager creates a workflow manager using an existing SQLite connection.
func NewManager(db *sql.DB) *Manager {
	return &Manager{db: db}
}

// CreateIfMissing creates a WorkflowInstance if one does not exist.
// Existing state is preserved if the record already exists.
func (m *Manager) CreateIfMissing(
	repo string, issueNum int, workflowName, currentState string,
) (*WorkflowInstance, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("workflow manager not initialized")
	}
	id := makeInstanceID(repo, issueNum, workflowName)
	if currentState == "" {
		currentState = ""
	}

	_, err := m.db.Exec(
		`INSERT INTO workflow_instances (id, workflow_name, repo, issue_num, current_state)
		 VALUES (?, ?, ?, ?, ?) ON CONFLICT (repo, issue_num, workflow_name) DO NOTHING`,
		id, workflowName, repo, issueNum, currentState,
	)
	if err != nil {
		return nil, fmt.Errorf("create workflow instance: %w", err)
	}
	return m.getByID(id)
}

// Advance writes a new transition and updates CurrentState atomically.
func (m *Manager) Advance(repo string, issueNum int, workflowName, fromState, toState, triggerAgent string) (*WorkflowInstance, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("workflow manager not initialized")
	}
	id := makeInstanceID(repo, issueNum, workflowName)

	tx, err := m.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin advance tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	now := time.Now().UTC()
	res, err := tx.Exec(
		`INSERT INTO workflow_transitions (workflow_instance_id, from_state, to_state, trigger_agent, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		id, fromState, toState, triggerAgent, now.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("insert transition: %w", err)
	}
	_ = res

	result, err := tx.Exec(
		`UPDATE workflow_instances
			SET current_state = ?, updated_at = ?
			WHERE id = ?`,
		toState, now.Format(time.RFC3339), id,
	)
	if err != nil {
		return nil, fmt.Errorf("update workflow instance state: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return nil, ErrWorkflowInstanceNotFound
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit advance tx: %w", err)
	}
	return m.getByID(id)
}

// QueryByRepoIssue returns all workflow instances for one issue with history.
func (m *Manager) QueryByRepoIssue(repo string, issueNum int) ([]WorkflowInstance, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("workflow manager not initialized")
	}
	rows, err := m.db.Query(
		`SELECT id, workflow_name, repo, issue_num, current_state, created_at, updated_at
		 FROM workflow_instances
		 WHERE repo = ? AND issue_num = ?
		 ORDER BY created_at, id`,
		repo, issueNum,
	)
	if err != nil {
		return nil, fmt.Errorf("query workflow instances: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkflowInstance
	for rows.Next() {
		inst := WorkflowInstance{}
		var createdAtStr, updatedAtStr string
		if err := rows.Scan(
			&inst.ID, &inst.WorkflowName, &inst.Repo, &inst.IssueNum, &inst.CurrentState,
			&createdAtStr, &updatedAtStr,
		); err != nil {
			return nil, fmt.Errorf("scan workflow instance: %w", err)
		}
		inst.CreatedAt = mustParseTime(createdAtStr)
		inst.UpdatedAt = mustParseTime(updatedAtStr)
		inst.History, err = m.queryHistory(inst.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow instances: %w", err)
	}
	return out, nil
}

// GetByID loads one WorkflowInstance and its transitions by ID.
func (m *Manager) GetByID(id string) (*WorkflowInstance, error) {
	return m.getByID(id)
}

func (m *Manager) getByID(id string) (*WorkflowInstance, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("workflow manager not initialized")
	}
	row := m.db.QueryRow(
		`SELECT id, workflow_name, repo, issue_num, current_state, created_at, updated_at
		 FROM workflow_instances
		 WHERE id = ?`,
		id,
	)
	record, err := scanInstance(row.Scan)
	if err == sql.ErrNoRows {
		return nil, ErrWorkflowInstanceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query workflow instance by id: %w", err)
	}
	record.History, err = m.queryHistory(record.ID)
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (m *Manager) queryHistory(instanceID string) ([]StateTransition, error) {
	rows, err := m.db.Query(
		`SELECT from_state, to_state, trigger_agent, created_at
		 FROM workflow_transitions
		 WHERE workflow_instance_id = ?
		 ORDER BY created_at ASC, id ASC`,
		instanceID,
	)
	if err != nil {
		return nil, fmt.Errorf("query workflow transitions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	history := make([]StateTransition, 0)
	for rows.Next() {
		var tr StateTransition
		var ts string
		if err := rows.Scan(&tr.From, &tr.To, &tr.TriggerAgent, &ts); err != nil {
			return nil, fmt.Errorf("scan workflow transition: %w", err)
		}
		tr.Timestamp = mustParseTime(ts)
		history = append(history, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow transitions: %w", err)
	}
	return history, nil
}

func scanInstance(scan func(dest ...any) error) (*WorkflowInstance, error) {
	record := &WorkflowInstance{}
	var createdAtStr, updatedAtStr string
	if err := scan(
		&record.ID, &record.WorkflowName, &record.Repo, &record.IssueNum,
		&record.CurrentState, &createdAtStr, &updatedAtStr,
	); err != nil {
		return nil, err
	}
	record.CreatedAt = mustParseTime(createdAtStr)
	record.UpdatedAt = mustParseTime(updatedAtStr)
	return record, nil
}

func mustParseTime(raw string) time.Time {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func makeInstanceID(repo string, issueNum int, workflowName string) string {
	return fmt.Sprintf("%s#%d#%s", repo, issueNum, workflowName)
}
