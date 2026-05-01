package store

import (
	"path/filepath"
	"testing"
)

// TestWorkerAuditURLRoundtrip exercises Phase 1 of the session-data
// ownership refactor (REQ-120): the workers table grew an audit_url
// column, and InsertWorker / QueryWorkers / GetWorker all carry it
// through.
func TestWorkerAuditURLRoundtrip(t *testing.T) {
	s := newTestStore(t)

	if err := s.InsertWorker(WorkerRecord{
		ID:       "w-audit",
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://worker-a:8091",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	got, err := s.GetWorker("w-audit")
	if err != nil || got == nil {
		t.Fatalf("GetWorker: %v %+v", err, got)
	}
	if got.AuditURL != "http://worker-a:8091" {
		t.Fatalf("audit_url = %q, want http://worker-a:8091", got.AuditURL)
	}

	// Re-register with a new audit_url and confirm it overwrites cleanly.
	if err := s.InsertWorker(WorkerRecord{
		ID:       "w-audit",
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://worker-a:9090",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker re-register: %v", err)
	}
	got2, err := s.GetWorker("w-audit")
	if err != nil || got2 == nil {
		t.Fatalf("GetWorker after re-register: %v %+v", err, got2)
	}
	if got2.AuditURL != "http://worker-a:9090" {
		t.Fatalf("audit_url after re-register = %q, want http://worker-a:9090", got2.AuditURL)
	}

	// QueryWorkers must also surface the column.
	rows, err := s.QueryWorkers("org/repo")
	if err != nil {
		t.Fatalf("QueryWorkers: %v", err)
	}
	if len(rows) != 1 || rows[0].AuditURL != "http://worker-a:9090" {
		t.Fatalf("QueryWorkers = %+v", rows)
	}
}

// TestWorkerAuditURLAddedByMigration verifies that opening a store created
// before the audit_url column existed (simulated here by dropping the
// column and reopening) results in the migration adding it back without
// destroying existing rows. This guards Phase 1 against losing rows when
// rolling out to existing deployments.
func TestWorkerAuditURLAddedByMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Seed a row with a non-empty audit_url so we can verify it survives
	// the simulated migration.
	if err := s.InsertWorker(WorkerRecord{
		ID:       "w-mig",
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://worker-a:8091",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	// Simulate the pre-migration shape: SQLite cannot DROP COLUMN without
	// recent versions, so we rebuild the table without audit_url and copy
	// the surviving columns. After this the row exists but has no
	// audit_url column at all.
	tx := []string{
		`CREATE TABLE workers_old (
			id TEXT PRIMARY KEY,
			repo TEXT NOT NULL,
			repos_json TEXT NOT NULL DEFAULT '[]',
			roles TEXT NOT NULL,
			runtime TEXT NOT NULL DEFAULT '',
			hostname TEXT,
			mgmt_base_url TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'online',
			token_kid TEXT,
			token_hash TEXT,
			token_revoked_at DATETIME,
			last_heartbeat DATETIME DEFAULT CURRENT_TIMESTAMP,
			registered_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO workers_old (id, repo, repos_json, roles, runtime, hostname, mgmt_base_url, status, token_kid, token_hash, token_revoked_at, last_heartbeat, registered_at)
			SELECT id, repo, repos_json, roles, runtime, hostname, mgmt_base_url, status, token_kid, token_hash, token_revoked_at, last_heartbeat, registered_at FROM workers`,
		`DROP TABLE workers`,
		`ALTER TABLE workers_old RENAME TO workers`,
	}
	for _, stmt := range tx {
		if _, err := s.DB().Exec(stmt); err != nil {
			t.Fatalf("simulate pre-migration: %s: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen: migration path must add audit_url back without dropping the row.
	s2, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore after pre-migration shape: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.GetWorker("w-mig")
	if err != nil || got == nil {
		t.Fatalf("GetWorker after migration: %v %+v", err, got)
	}
	if got.ID != "w-mig" || got.Repo != "org/repo" {
		t.Fatalf("row mutated: %+v", got)
	}
	// Pre-migration rows should default to empty audit_url after the
	// ALTER TABLE ADD COLUMN ... DEFAULT '' migration runs.
	if got.AuditURL != "" {
		t.Fatalf("audit_url after migration = %q, want empty default", got.AuditURL)
	}

	// Subsequent UPSERT must cleanly write a new audit_url.
	if err := s2.InsertWorker(WorkerRecord{
		ID:       "w-mig",
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://worker-a:9100",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker after migration: %v", err)
	}
	final, err := s2.GetWorker("w-mig")
	if err != nil || final == nil {
		t.Fatalf("GetWorker final: %v %+v", err, final)
	}
	if final.AuditURL != "http://worker-a:9100" {
		t.Fatalf("audit_url final = %q", final.AuditURL)
	}
}
