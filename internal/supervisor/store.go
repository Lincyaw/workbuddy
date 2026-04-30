package supervisor

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // sqlite driver
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS agents (
    agent_id    TEXT PRIMARY KEY,
    pid         INTEGER NOT NULL,
    start_ticks INTEGER NOT NULL,
    started_at  TEXT NOT NULL,
    status      TEXT NOT NULL,
    exit_code   INTEGER,
    session_id  TEXT NOT NULL DEFAULT '',
    runtime     TEXT NOT NULL DEFAULT '',
    workdir     TEXT NOT NULL DEFAULT '',
    stdout_path TEXT NOT NULL DEFAULT '',
    stderr_path TEXT NOT NULL DEFAULT ''
);
`

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open supervisor db: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init supervisor schema: %w", err)
	}
	return db, nil
}

type agentRow struct {
	AgentID    string
	PID        int
	StartTicks uint64
	StartedAt  time.Time
	Status     string
	ExitCode   *int
	SessionID  string
	Runtime    string
	Workdir    string
	StdoutPath string
	StderrPath string
}

func insertAgent(db *sql.DB, r agentRow) error {
	_, err := db.Exec(
		`INSERT INTO agents(agent_id,pid,start_ticks,started_at,status,session_id,runtime,workdir,stdout_path,stderr_path)
         VALUES(?,?,?,?,?,?,?,?,?,?)`,
		r.AgentID, r.PID, int64(r.StartTicks), r.StartedAt.UTC().Format(time.RFC3339Nano),
		r.Status, r.SessionID, r.Runtime, r.Workdir, r.StdoutPath, r.StderrPath,
	)
	return err
}

func updateAgentExit(db *sql.DB, agentID string, exitCode int) error {
	_, err := db.Exec(
		`UPDATE agents SET status='exited', exit_code=? WHERE agent_id=?`,
		exitCode, agentID,
	)
	return err
}

func loadAgents(db *sql.DB) ([]agentRow, error) {
	rows, err := db.Query(`SELECT agent_id,pid,start_ticks,started_at,status,exit_code,session_id,runtime,workdir,stdout_path,stderr_path FROM agents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agentRow
	for rows.Next() {
		var (
			r       agentRow
			started string
			ec      sql.NullInt64
			ticks   int64
		)
		if err := rows.Scan(&r.AgentID, &r.PID, &ticks, &started, &r.Status, &ec, &r.SessionID, &r.Runtime, &r.Workdir, &r.StdoutPath, &r.StderrPath); err != nil {
			return nil, err
		}
		r.StartTicks = uint64(ticks)
		if t, err := time.Parse(time.RFC3339Nano, started); err == nil {
			r.StartedAt = t
		}
		if ec.Valid {
			v := int(ec.Int64)
			r.ExitCode = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
