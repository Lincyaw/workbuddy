package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Pipeline hazard kinds. These name configuration-incompleteness conditions
// that cause the coordinator to silently skip an issue. See REQ #255.
const (
	// HazardKindNoWorkflowMatch — issue carries a status:* label but no
	// workflow trigger label matched, so the state machine cannot enter the
	// issue and the user is left with no audit trail.
	HazardKindNoWorkflowMatch = "no-workflow-match"
	// HazardKindAwaitingStatusLabel — issue carries a workflow trigger label
	// and a depends_on declaration but no status:* label, so the state
	// machine never enters the issue and the dependency gate cannot release
	// downstream work.
	HazardKindAwaitingStatusLabel = "awaiting-status-label"
)

// PipelineHazard records a per-issue configuration-incompleteness condition.
type PipelineHazard struct {
	Repo        string
	IssueNum    int
	Kind        string
	Fingerprint string
	DetectedAt  time.Time
}

// UpsertIssuePipelineHazard inserts or updates the hazard row for the given
// (repo, issue_num). It returns changed=true when the row was created or its
// kind/fingerprint differs from a prior row, so callers can decide whether to
// emit a fresh INFO event (idempotency contract).
func (s *Store) UpsertIssuePipelineHazard(h PipelineHazard) (changed bool, err error) {
	prev, err := s.QueryIssuePipelineHazard(h.Repo, h.IssueNum)
	if err != nil {
		return false, err
	}
	now := s.now().UTC().Format("2006-01-02 15:04:05")
	_, err = s.db.Exec(
		`INSERT INTO issue_pipeline_hazards
			(repo, issue_num, kind, fingerprint, detected_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(repo, issue_num) DO UPDATE SET
				kind = excluded.kind,
				fingerprint = excluded.fingerprint,
				detected_at = CASE
					WHEN issue_pipeline_hazards.kind = excluded.kind
						AND issue_pipeline_hazards.fingerprint = excluded.fingerprint
					THEN issue_pipeline_hazards.detected_at
					ELSE excluded.detected_at
				END`,
		h.Repo, h.IssueNum, h.Kind, h.Fingerprint, now,
	)
	if err != nil {
		return false, fmt.Errorf("store: upsert issue pipeline hazard: %w", err)
	}
	if prev == nil {
		return true, nil
	}
	if prev.Kind != h.Kind || prev.Fingerprint != h.Fingerprint {
		return true, nil
	}
	return false, nil
}

// QueryIssuePipelineHazard returns the hazard for the given issue, or nil if
// none is recorded.
func (s *Store) QueryIssuePipelineHazard(repo string, issueNum int) (*PipelineHazard, error) {
	row := s.db.QueryRow(
		`SELECT repo, issue_num, kind, fingerprint, detected_at
			FROM issue_pipeline_hazards WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	)
	var (
		out    PipelineHazard
		rawTS  string
	)
	err := row.Scan(&out.Repo, &out.IssueNum, &out.Kind, &out.Fingerprint, &rawTS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: query issue pipeline hazard: %w", err)
	}
	if ts, ok := ParseTimestamp(rawTS, "issue_pipeline_hazards.detected_at"); ok {
		out.DetectedAt = ts.UTC()
	}
	return &out, nil
}

// ListIssuePipelineHazards returns every hazard for the given repo, or every
// hazard across all repos when repo is empty.
func (s *Store) ListIssuePipelineHazards(repo string) ([]PipelineHazard, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if repo == "" {
		rows, err = s.db.Query(
			`SELECT repo, issue_num, kind, fingerprint, detected_at
				FROM issue_pipeline_hazards ORDER BY repo, issue_num`,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT repo, issue_num, kind, fingerprint, detected_at
				FROM issue_pipeline_hazards WHERE repo = ? ORDER BY issue_num`,
			repo,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list issue pipeline hazards: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PipelineHazard
	for rows.Next() {
		var h PipelineHazard
		var rawTS string
		if err := rows.Scan(&h.Repo, &h.IssueNum, &h.Kind, &h.Fingerprint, &rawTS); err != nil {
			return nil, fmt.Errorf("store: scan issue pipeline hazard: %w", err)
		}
		if ts, ok := ParseTimestamp(rawTS, "issue_pipeline_hazards.detected_at"); ok {
			h.DetectedAt = ts.UTC()
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ClearIssuePipelineHazard deletes the hazard row for the given issue. Returns
// nil when the row does not exist.
func (s *Store) ClearIssuePipelineHazard(repo string, issueNum int) error {
	_, err := s.db.Exec(
		`DELETE FROM issue_pipeline_hazards WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	)
	if err != nil {
		return fmt.Errorf("store: clear issue pipeline hazard: %w", err)
	}
	return nil
}
