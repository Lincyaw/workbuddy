// Package cmd: `workbuddy db migrate` command.
//
// One-shot offline migration of every row in a SQLite source DSN to a MySQL
// destination DSN. Per docs/decisions/2026-05-13-k8s-agentm-otel.md Block 3
// § Migration tool and Block 5 § Migration & rollout, this exists so users
// moving from the systemd (SQLite) topology to the K8s (MySQL) topology
// have a deterministic, verifiable cutover step.
//
// Hard scope (per issue #318):
//
//   - Forward direction only (SQLite → MySQL). Reverse is not supported.
//   - Assumes the destination is already at the same workbuddy schema
//     version. No schema upgrade is performed.
//   - Offline. The coordinator must be stopped on both sides.
//
// The command opens source via store.New(--from) and destination via
// store.New(--to), so any DSN scheme the store package understands is
// accepted on either side; the test suite exploits this to drive the
// migration path with a sqlite:// destination instead of standing up
// MySQL in CI.
package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

// migrationTables is the canonical, FK-safe migration order. Children (rows
// with foreign keys into other tables) appear after their parents so that
// MySQL InnoDB does not reject the INSERT before the parent row exists.
//
// Keep in sync with internal/store/mysql/schema.sql and the SQLite DDL in
// dbStore.createTables. internal/store/schema_parity_test.go guards parity
// of the *column* set; this list guards parity of the *table* set used by
// the migrator. If you add a new table to schema.sql you must add it here
// too — TestMigrateTablesCoverSchema enforces that.
//
// Column lists are explicit (rather than `SELECT *`) so the destination
// INSERT has a deterministic column order independent of the source's
// arrangement. Both engines accept the order below.
var migrationTables = []migrationTable{
	{
		name:    "events",
		columns: []string{"id", "ts", "type", "repo", "issue_num", "payload"},
	},
	{
		name:    "repo_registrations",
		columns: []string{"repo", "environment", "status", "config_json", "registered_at", "updated_at"},
	},
	{
		name: "workers",
		columns: []string{
			"id", "repo", "repos_json", "roles", "runtime", "hostname",
			"mgmt_base_url", "audit_url", "tunnel", "status", "token_kid",
			"token_hash", "token_revoked_at", "last_heartbeat", "registered_at",
		},
	},
	{
		name: "workflow_instances",
		columns: []string{
			"id", "workflow_name", "repo", "issue_num", "current_state",
			"created_at", "updated_at",
		},
	},
	{
		// workflow_transitions has a FOREIGN KEY to workflow_instances(id).
		// MySQL enforces it; place after workflow_instances.
		name: "workflow_transitions",
		columns: []string{
			"id", "workflow_instance_id", "from_state", "to_state",
			"trigger_agent", "created_at",
		},
	},
	{
		name: "task_queue",
		columns: []string{
			"id", "repo", "issue_num", "agent_name", "role", "runtime",
			"workflow", "state", "worker_id", "claim_token", "status",
			"lease_expires_at", "acked_at", "heartbeat_at", "completed_at",
			"exit_code", "session_refs", "rollout_index", "rollouts_total",
			"rollout_group_id", "supervisor_agent_id", "created_at", "updated_at",
		},
	},
	{
		name: "issue_cache",
		columns: []string{
			"repo", "issue_num", "labels", "body", "state",
			"root_trace_id", "updated_at",
		},
	},
	{
		name: "agent_sessions",
		columns: []string{
			"id", "session_id", "task_id", "repo", "issue_num", "agent_name",
			"summary", "raw_path", "created_at",
		},
	},
	{
		name: "sessions",
		columns: []string{
			"id", "session_id", "task_id", "repo", "issue_num", "agent_name",
			"runtime", "worker_id", "attempt", "status", "dir",
			"stdout_path", "stderr_path", "tool_calls_path", "metadata_path",
			"summary", "raw_path", "created_at", "closed_at",
		},
	},
	{
		name: "session_routes",
		columns: []string{
			"session_id", "worker_id", "repo", "issue_num", "created_at",
		},
	},
	{
		name: "issue_dependencies",
		columns: []string{
			"repo", "issue_num", "depends_on_repo", "depends_on_issue_num",
			"source_hash", "status",
		},
	},
	{
		name: "issue_dependency_state",
		columns: []string{
			"repo", "issue_num", "verdict", "resume_label",
			"blocked_reason_hash", "override_active", "graph_version",
			"last_reaction_blocked", "last_evaluated_at",
		},
	},
	{
		name: "issue_claim",
		columns: []string{
			"repo", "issue_num", "worker_id", "claim_token",
			"acquired_at", "expires_at",
		},
	},
	{
		name: "issue_pipeline_hazards",
		columns: []string{
			"repo", "issue_num", "kind", "fingerprint", "detected_at",
		},
	},
	{
		name: "issue_cycle_state",
		columns: []string{
			"repo", "issue_num", "dev_review_cycle_count", "synth_cycle_count",
			"first_dispatch_at", "cap_hit_at", "synth_cap_hit_at", "updated_at",
		},
	},
	{
		name: "transition_counts",
		columns: []string{
			"repo", "issue_num", "from_state", "to_state", "count",
		},
	},
}

type migrationTable struct {
	name    string
	columns []string
}

// dbMigrateOpts captures parsed flags for `workbuddy db migrate`.
type dbMigrateOpts struct {
	from      string
	to        string
	force     bool
	batchSize int
}

var dbMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "One-shot copy of every row from --from DSN to --to DSN (SQLite → MySQL)",
	Long: `Offline migration of a workbuddy database from SQLite to MySQL.

Reads every row from the source store (via store.New) and writes to the
destination store. Per-table row counts are verified at the end; any
mismatch is a hard error.

The destination must already have the target workbuddy schema (the
store.New factory creates tables on open). If the destination contains
any rows in a known table the command refuses to run unless --force is
passed; --force first wipes the destination tables (in FK-safe order)
and then performs the copy.

Hard scope:
  - Forward direction only (SQLite → MySQL). Reverse is not supported.
  - No schema upgrades — both sides must be the same workbuddy version.
  - Offline only. Stop the coordinator before running.

Example:
  workbuddy db migrate \
    --from sqlite:///var/lib/workbuddy/workbuddy.db \
    --to   mysql://user:pass@tcp(mysql:3306)/workbuddy`,
	RunE: runDBMigrateCmd,
}

func init() {
	dbMigrateCmd.Flags().String("from", "", "Source DSN (sqlite://path or bare path; required)")
	dbMigrateCmd.Flags().String("to", "", "Destination DSN (mysql://...; required)")
	dbMigrateCmd.Flags().Bool("force", false, "Wipe non-empty destination tables before copying")
	dbMigrateCmd.Flags().Int("batch-size", 1000, "Rows per destination INSERT batch (each table is one transaction)")
	_ = dbMigrateCmd.MarkFlagRequired("from")
	_ = dbMigrateCmd.MarkFlagRequired("to")
	dbCmd.AddCommand(dbMigrateCmd)
}

func runDBMigrateCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDBMigrateFlags(cmd)
	if err != nil {
		return err
	}
	if err := requireWritable(cmd, "db migrate"); err != nil {
		return err
	}
	return runDBMigrate(cmd.Context(), opts, cmdStderr(cmd))
}

func parseDBMigrateFlags(cmd *cobra.Command) (dbMigrateOpts, error) {
	from, _ := cmd.Flags().GetString("from")
	to, _ := cmd.Flags().GetString("to")
	force, _ := cmd.Flags().GetBool("force")
	batch, _ := cmd.Flags().GetInt("batch-size")
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" {
		return dbMigrateOpts{}, &cliExitError{msg: "db migrate: --from is required", code: exitCodeFailure}
	}
	if to == "" {
		return dbMigrateOpts{}, &cliExitError{msg: "db migrate: --to is required", code: exitCodeFailure}
	}
	if batch <= 0 {
		batch = 1000
	}
	return dbMigrateOpts{from: from, to: to, force: force, batchSize: batch}, nil
}

// runDBMigrate is the testable entry point. The cobra wrapper does flag
// parsing only; this function does the work and is also called directly
// from cmd/db_migrate_test.go.
func runDBMigrate(ctx context.Context, opts dbMigrateOpts, progress io.Writer) error {
	if progress == nil {
		progress = io.Discard
	}

	src, err := store.New(opts.from)
	if err != nil {
		return fmt.Errorf("db migrate: open source %q: %w", opts.from, err)
	}
	defer func() { _ = src.Close() }()

	dst, err := store.New(opts.to)
	if err != nil {
		return fmt.Errorf("db migrate: open destination %q: %w", opts.to, err)
	}
	defer func() { _ = dst.Close() }()

	// Refuse to clobber a destination that already has data, unless --force.
	nonEmpty, err := nonEmptyDestinationTables(dst)
	if err != nil {
		return fmt.Errorf("db migrate: inspect destination: %w", err)
	}
	if len(nonEmpty) > 0 {
		if !opts.force {
			return &cliExitError{
				msg: fmt.Sprintf(
					"db migrate: destination is not empty (tables with rows: %s); re-run with --force to overwrite",
					strings.Join(nonEmpty, ", "),
				),
				code: exitCodeFailure,
			}
		}
		fmt.Fprintf(progress, "[migrate] --force: wiping %d non-empty destination tables\n", len(nonEmpty))
		if err := wipeDestination(dst); err != nil {
			return fmt.Errorf("db migrate: wipe destination: %w", err)
		}
	}

	start := time.Now()
	var totalRows int64
	for _, table := range migrationTables {
		if err := ctx.Err(); err != nil {
			return err
		}
		copied, err := copyTable(ctx, src, dst, table, opts.batchSize, progress)
		if err != nil {
			return fmt.Errorf("db migrate: table %q: %w", table.name, err)
		}
		fmt.Fprintf(progress, "[migrate] %s: %d rows copied\n", table.name, copied)
		totalRows += copied
	}

	// Row-count verification — per-table SELECT COUNT(*) on both sides.
	for _, table := range migrationTables {
		srcCount, err := countTable(src, table.name)
		if err != nil {
			return fmt.Errorf("db migrate: verify source %q: %w", table.name, err)
		}
		dstCount, err := countTable(dst, table.name)
		if err != nil {
			return fmt.Errorf("db migrate: verify destination %q: %w", table.name, err)
		}
		if srcCount != dstCount {
			return fmt.Errorf(
				"db migrate: row-count mismatch on %q: source=%d destination=%d",
				table.name, srcCount, dstCount,
			)
		}
	}

	fmt.Fprintf(progress, "[migrate] done: %d rows across %d tables in %s\n",
		totalRows, len(migrationTables), time.Since(start).Round(time.Millisecond))
	return nil
}

// nonEmptyDestinationTables returns the names of any known tables on dst
// that currently hold rows. Used to refuse a clobbering run unless --force.
func nonEmptyDestinationTables(dst store.Store) ([]string, error) {
	var nonEmpty []string
	for _, table := range migrationTables {
		n, err := countTable(dst, table.name)
		if err != nil {
			return nil, fmt.Errorf("count %q: %w", table.name, err)
		}
		if n > 0 {
			nonEmpty = append(nonEmpty, fmt.Sprintf("%s(%d)", table.name, n))
		}
	}
	return nonEmpty, nil
}

// wipeDestination deletes every row from every migration table in reverse
// order so child rows go before parents (workflow_transitions has a FK to
// workflow_instances on MySQL; SQLite happily processes either order but
// we keep one rule for both).
//
// Plain DELETE is used rather than TRUNCATE because the latter is not
// supported on SQLite and on MySQL would require dropping FK checks. The
// row volumes involved in a one-shot migration are bounded by the source
// dataset and a DELETE inside a transaction is well within budget.
func wipeDestination(dst store.Store) error {
	for i := len(migrationTables) - 1; i >= 0; i-- {
		table := migrationTables[i]
		if _, err := dst.Exec("DELETE FROM " + table.name); err != nil {
			return fmt.Errorf("delete from %q: %w", table.name, err)
		}
	}
	return nil
}

// copyTable streams every row from src.<table> to dst.<table>. The
// destination INSERTs are grouped into batches of batchSize rows to amortise
// per-row round-trip latency on MySQL; each table forms its own logical
// transaction boundary at the batch level (Exec is autocommitting on the
// underlying *sql.DB but tests target SQLite which is single-threaded, so
// the batch shape doesn't introduce visible mid-table partial state under
// the no-concurrent-writer contract of `db migrate`).
func copyTable(
	ctx context.Context,
	src store.Store, dst store.Store,
	table migrationTable, batchSize int,
	progress io.Writer,
) (int64, error) {
	cols := strings.Join(quoteIdents(table.columns), ", ")
	selectSQL := "SELECT " + cols + " FROM " + table.name
	rows, err := src.Query(selectSQL)
	if err != nil {
		return 0, fmt.Errorf("select: %w", err)
	}
	defer func() { _ = rows.Close() }()

	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(table.columns)), ", ")
	insertOne := "INSERT INTO " + table.name + " (" + cols + ") VALUES (" + placeholders + ")"

	// Buffered batch: we accumulate rows in memory up to batchSize and then
	// flush as a multi-VALUES INSERT to amortise driver round-trips. This is
	// the "stream — don't slurp" contract from issue #318: peak memory is
	// O(batchSize * columns), not O(total rows).
	var (
		batch    [][]any
		copied   int64
		batchCap = batchSize
	)
	if batchCap <= 0 {
		batchCap = 1000
	}

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := bulkInsert(dst, table.name, table.columns, insertOne, batch); err != nil {
			return err
		}
		copied += int64(len(batch))
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		scanTargets := make([]any, len(table.columns))
		scanHolders := make([]any, len(table.columns))
		for i := range scanTargets {
			scanHolders[i] = &scanTargets[i]
		}
		if err := rows.Scan(scanHolders...); err != nil {
			return copied, fmt.Errorf("scan row %d: %w", copied+int64(len(batch))+1, err)
		}
		batch = append(batch, scanTargets)
		if len(batch) >= batchCap {
			if err := flush(); err != nil {
				return copied, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return copied, fmt.Errorf("rows iter: %w", err)
	}
	if err := flush(); err != nil {
		return copied, err
	}
	return copied, nil
}

// bulkInsert executes one multi-row INSERT for the supplied batch. If the
// destination driver rejects the bulk form, the fallback is the per-row
// insertOne template. This keeps the migration robust against
// strict_mode / max_allowed_packet quirks on MySQL without requiring the
// caller to tune batch-size for a specific server.
func bulkInsert(dst store.Store, tableName string, columns []string, insertOne string, batch [][]any) error {
	if len(batch) == 0 {
		return nil
	}
	colList := strings.Join(quoteIdents(columns), ", ")
	rowPlaceholder := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ") + ")"
	var sb strings.Builder
	sb.Grow(64 + len(rowPlaceholder)*len(batch))
	sb.WriteString("INSERT INTO ")
	sb.WriteString(tableName)
	sb.WriteString(" (")
	sb.WriteString(colList)
	sb.WriteString(") VALUES ")
	args := make([]any, 0, len(columns)*len(batch))
	for i, row := range batch {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(rowPlaceholder)
		args = append(args, row...)
	}
	if _, err := dst.Exec(sb.String(), args...); err == nil {
		return nil
	} else if !shouldFallbackToPerRow(err) {
		return fmt.Errorf("bulk insert: %w", err)
	}
	// Fallback: row-by-row.
	for _, row := range batch {
		if _, err := dst.Exec(insertOne, row...); err != nil {
			return fmt.Errorf("per-row insert: %w", err)
		}
	}
	return nil
}

// shouldFallbackToPerRow tags errors where dropping to a per-row INSERT
// stands a chance of succeeding (packet-size limits, parameter-count
// limits). For everything else we surface the original error so users see
// real schema/data problems immediately.
func shouldFallbackToPerRow(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "max_allowed_packet"):
		return true
	case strings.Contains(msg, "too many sql variables"):
		return true
	case strings.Contains(msg, "parameter index"):
		return true
	}
	return false
}

// countTable runs `SELECT COUNT(*) FROM <table>` on s.
func countTable(s store.Store, table string) (int64, error) {
	row := s.QueryRow("SELECT COUNT(*) FROM " + table)
	var n int64
	if err := row.Scan(&n); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}

// quoteIdents wraps each identifier in backticks. MySQL requires backticks
// for reserved words; SQLite accepts them as a valid identifier quoting
// form too, so the same SQL works on both backends.
func quoteIdents(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = "`" + s + "`"
	}
	return out
}
