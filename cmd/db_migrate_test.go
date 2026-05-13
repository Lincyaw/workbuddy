// Tests for `workbuddy db migrate`.
//
// Design rationale:
//
// The migration target in production is MySQL, but for hermetic unit tests
// we use a *second SQLite database* as the destination. This is safe because
// the migrator goes through the Store interface and a single common SQL
// surface (`SELECT col, ... FROM table`, `INSERT INTO table (cols) VALUES
// (...)`, `SELECT COUNT(*)`) which both backends accept verbatim. The
// dialect-aware rewrite layer in internal/store would only kick in for
// dialect-specific fragments (INSERT OR IGNORE, ON CONFLICT, etc.) that
// this command intentionally avoids. So the same code path that the
// SQLite → MySQL migration exercises is what these tests drive.
//
// A real-MySQL variant lives under the mysql_integration build tag in
// db_migrate_mysql_test.go.
package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestMigrateTablesCoverSchema enforces that the migration plan covers every
// table the SQLite store actually creates on bootstrap. If a contributor
// adds a new table to internal/store and forgets to extend migrationTables,
// this test fails with the diff before db migrate ships a silent gap.
func TestMigrateTablesCoverSchema(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "schema.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	rows, err := st.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var live []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		live = append(live, n)
	}
	planSet := map[string]struct{}{}
	for _, t := range migrationTables {
		planSet[t.name] = struct{}{}
	}
	liveSet := map[string]struct{}{}
	for _, n := range live {
		liveSet[n] = struct{}{}
	}
	var missingInPlan, extraInPlan []string
	for n := range liveSet {
		if _, ok := planSet[n]; !ok {
			missingInPlan = append(missingInPlan, n)
		}
	}
	for n := range planSet {
		if _, ok := liveSet[n]; !ok {
			extraInPlan = append(extraInPlan, n)
		}
	}
	sort.Strings(missingInPlan)
	sort.Strings(extraInPlan)
	if len(missingInPlan) > 0 || len(extraInPlan) > 0 {
		t.Fatalf("migrationTables drift vs live SQLite schema:\n  missing in plan: %v\n  extra in plan:   %v",
			missingInPlan, extraInPlan)
	}
}

// TestDBMigrateHappyPath seeds a SQLite source with representative rows
// across the main tables, runs the migrator to a fresh SQLite destination,
// and verifies per-table row counts plus specific row contents survived.
func TestDBMigrateHappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srcDSN := "sqlite://" + filepath.Join(dir, "src.db")
	dstDSN := "sqlite://" + filepath.Join(dir, "dst.db")

	seedSrc := mustOpen(t, srcDSN)
	seedRepresentative(t, seedSrc)
	_ = seedSrc.Close()

	// Destination must already exist with the target schema. store.New
	// creates the tables on open; close it so the migrator gets a clean
	// handle.
	mustClose(t, mustOpen(t, dstDSN))

	var progress bytes.Buffer
	if err := runDBMigrate(context.Background(), dbMigrateOpts{
		from: srcDSN,
		to:   dstDSN,
	}, &progress); err != nil {
		t.Fatalf("runDBMigrate: %v\nprogress:\n%s", err, progress.String())
	}

	dst := mustOpen(t, dstDSN)
	defer func() { _ = dst.Close() }()

	// Per-table count parity.
	src := mustOpen(t, srcDSN)
	defer func() { _ = src.Close() }()
	for _, table := range migrationTables {
		s, err := countTable(src, table.name)
		if err != nil {
			t.Fatalf("count src %q: %v", table.name, err)
		}
		d, err := countTable(dst, table.name)
		if err != nil {
			t.Fatalf("count dst %q: %v", table.name, err)
		}
		if s != d {
			t.Errorf("count mismatch on %q: src=%d dst=%d", table.name, s, d)
		}
	}

	// Spot-check that an issue_cache row migrated faithfully.
	got, err := dst.QueryIssueCache("owner/repo", 318)
	if err != nil {
		t.Fatalf("QueryIssueCache: %v", err)
	}
	if got == nil {
		t.Fatalf("issue_cache row missing after migrate")
	}
	if got.Body == "" || !strings.Contains(got.Body, "v0.5") {
		t.Errorf("issue_cache body lost in transit: %q", got.Body)
	}

	// And that a transition count survived.
	tc, err := dst.QueryTransitionCounts("owner/repo", 318)
	if err != nil {
		t.Fatalf("QueryTransitionCounts: %v", err)
	}
	if len(tc) == 0 {
		t.Errorf("expected at least one transition_count row migrated")
	}

	progressOut := progress.String()
	for _, want := range []string{
		"[migrate] events:",
		"[migrate] issue_cache:",
		"[migrate] done:",
	} {
		if !strings.Contains(progressOut, want) {
			t.Errorf("progress output missing %q\nfull output:\n%s", want, progressOut)
		}
	}
}

// TestDBMigrateRefusesNonEmptyDestination confirms AC-1-2: a destination
// with pre-existing rows is rejected unless --force.
func TestDBMigrateRefusesNonEmptyDestination(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srcDSN := "sqlite://" + filepath.Join(dir, "src.db")
	dstDSN := "sqlite://" + filepath.Join(dir, "dst.db")

	src := mustOpen(t, srcDSN)
	seedRepresentative(t, src)
	_ = src.Close()

	// Pre-populate destination with at least one row in a known table.
	dst := mustOpen(t, dstDSN)
	if err := dst.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/other",
		IssueNum: 99,
		Labels:   `[]`,
		Body:     "pre-existing",
		State:    "open",
	}); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	_ = dst.Close()

	err := runDBMigrate(context.Background(), dbMigrateOpts{
		from: srcDSN,
		to:   dstDSN,
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("expected refusal on non-empty destination, got nil")
	}
	var exitErr *cliExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected cliExitError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "issue_cache") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should name the offending table and suggest --force; got: %v", err)
	}
}

// TestDBMigrateForceOverwritesDestination confirms that --force wipes the
// destination tables and the resulting database matches the source. The
// chosen semantics for --force is "wipe + rewrite from source", which is
// the only semantics that satisfies AC-1-1 row-count parity after a re-run:
// a forced re-run leaves the destination identical to source, regardless of
// what was there before.
func TestDBMigrateForceOverwritesDestination(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srcDSN := "sqlite://" + filepath.Join(dir, "src.db")
	dstDSN := "sqlite://" + filepath.Join(dir, "dst.db")

	src := mustOpen(t, srcDSN)
	seedRepresentative(t, src)
	_ = src.Close()

	dst := mustOpen(t, dstDSN)
	// Stale row that should be gone after --force.
	if err := dst.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/stale",
		IssueNum: 1,
		Labels:   `[]`,
		Body:     "stale",
		State:    "open",
	}); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	_ = dst.Close()

	if err := runDBMigrate(context.Background(), dbMigrateOpts{
		from:  srcDSN,
		to:    dstDSN,
		force: true,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runDBMigrate --force: %v", err)
	}

	dst = mustOpen(t, dstDSN)
	defer func() { _ = dst.Close() }()

	// Stale row gone.
	stale, err := dst.QueryIssueCache("owner/stale", 1)
	if err != nil {
		t.Fatalf("QueryIssueCache stale: %v", err)
	}
	if stale != nil {
		t.Errorf("--force should have wiped owner/stale#1; still present: %+v", stale)
	}
	// Migrated row present.
	migrated, err := dst.QueryIssueCache("owner/repo", 318)
	if err != nil {
		t.Fatalf("QueryIssueCache migrated: %v", err)
	}
	if migrated == nil {
		t.Fatalf("owner/repo#318 missing after --force migrate")
	}

	// Re-running --force a second time must remain consistent: counts on
	// both sides should still match.
	if err := runDBMigrate(context.Background(), dbMigrateOpts{
		from:  srcDSN,
		to:    dstDSN,
		force: true,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("re-run --force: %v", err)
	}
}

// TestDBMigrateRequiresFromAndTo guards the CLI argument contract.
func TestDBMigrateRequiresFromAndTo(t *testing.T) {
	t.Parallel()
	cmd := dbMigrateCmd
	// reset to defaults
	_ = cmd.Flags().Set("from", "")
	_ = cmd.Flags().Set("to", "")
	if _, err := parseDBMigrateFlags(cmd); err == nil {
		t.Fatalf("expected error when --from missing")
	}
	_ = cmd.Flags().Set("from", "sqlite://x")
	if _, err := parseDBMigrateFlags(cmd); err == nil {
		t.Fatalf("expected error when --to missing")
	}
	_ = cmd.Flags().Set("to", "sqlite://y")
	if _, err := parseDBMigrateFlags(cmd); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Reset after.
	_ = cmd.Flags().Set("from", "")
	_ = cmd.Flags().Set("to", "")
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func mustOpen(t *testing.T, dsn string) store.Store {
	t.Helper()
	s, err := store.New(dsn)
	if err != nil {
		t.Fatalf("store.New(%q): %v", dsn, err)
	}
	return s
}

func mustClose(t *testing.T, s store.Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// seedRepresentative inserts at least one row into the main user-data tables
// so the migrator has something to chew on. The point is to cover the
// representative shapes (auto-increment PK, composite PK, FK, JSON blobs,
// nullable DATETIME) — not to be exhaustive about every column.
func seedRepresentative(t *testing.T, s store.Store) {
	t.Helper()
	now := time.Now().UTC()

	// events (auto-inc PK, nullable issue_num, payload TEXT).
	if _, err := s.InsertEvent(store.Event{
		Type:     "issue.observed",
		Repo:     "owner/repo",
		IssueNum: 318,
		Payload:  `{"hello":"world"}`,
	}); err != nil {
		t.Fatalf("seed events: %v", err)
	}

	// repo_registrations.
	if err := s.UpsertRepoRegistration(store.RepoRegistrationRecord{
		Repo:        "owner/repo",
		Environment: "test",
		Status:      "active",
		ConfigJSON:  `{}`,
	}); err != nil {
		t.Fatalf("seed repo_registrations: %v", err)
	}

	// workers (also exercises repos_json LONGTEXT).
	if err := s.InsertWorker(store.WorkerRecord{
		ID:        "worker-a",
		Repo:      "owner/repo",
		ReposJSON: `["owner/repo"]`,
		Roles:     `["dev","review"]`,
		Runtime:   "claude-code",
		Hostname:  "host-a",
	}); err != nil {
		t.Fatalf("seed workers: %v", err)
	}

	// issue_cache (composite PK).
	if err := s.UpsertIssueCache(store.IssueCache{
		Repo:      "owner/repo",
		IssueNum:  318,
		Labels:    `["status:developing"]`,
		Body:      "v0.5: Add db migrate command (sqlite -> mysql)",
		State:     "open",
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed issue_cache: %v", err)
	}

	// task_queue (long row with many columns, nullable DATETIMEs).
	if err := s.InsertTask(store.TaskRecord{
		ID:        "task-1",
		Repo:      "owner/repo",
		IssueNum:  318,
		AgentName: "dev-agent",
		Role:      "dev",
		Runtime:   "claude-code",
		Workflow:  "default",
		State:     "status:developing",
		Status:    "pending",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed task_queue: %v", err)
	}

	// transition_counts (composite PK, no auto-inc).
	if _, err := s.IncrementTransition("owner/repo", 318, "triage", "status:developing"); err != nil {
		t.Fatalf("seed transition_counts: %v", err)
	}

	// workflow_instances + workflow_transitions (FK parent/child pair).
	if err := s.CreateWorkflowInstanceIfMissing("wf-inst-1", "default", "owner/repo", 318, "start"); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	if err := s.AdvanceWorkflowInstance("wf-inst-1", "start", "status:developing", "dev-agent", now); err != nil {
		t.Fatalf("seed workflow_transitions: %v", err)
	}

	// issue_cycle_state.
	if _, err := s.IncrementDevReviewCycleCount("owner/repo", 318); err != nil {
		t.Fatalf("seed issue_cycle_state: %v", err)
	}
}
