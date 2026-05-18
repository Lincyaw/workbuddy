// Tests for the migrate-time timestamp rewriter (REQ-156, #345).
//
// The default `go test ./...` suite drives a SQLite → SQLite migration so
// the rewriter's parse-then-RFC3339 path is identity-preserving — exactly
// the contract we want to pin: when both sides are SQLite the migration
// must not perturb the stored layout (otherwise a forced re-run would
// flip text bytes unnecessarily and downstream lexicographic WHERE
// cutoffs would mis-sort).
//
// The MySQL leg (where the rewriter's value is to convert RFC3339 to the
// MySQL `DATETIME(6)` space-form) is exercised end-to-end under build
// tag `mysql_integration` in db_migrate_mysql_test.go.

package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestMigrateTimestampColumnsCoverSchema enforces parity between the
// timestamp-column flags on migrationTable and the DATETIME(6)
// declarations in internal/store/mysql/schema.sql. If a contributor adds
// a DATETIME column to the MySQL schema and forgets to flag it here, the
// migrate path would scan the value as a raw RFC3339 string and the
// MySQL INSERT would be rejected under STRICT_TRANS_TABLES. The test
// closes that gap statically.
//
// The schema file is the source of truth because TestSchemaParity in
// internal/store keeps the SQLite DDL aligned with it.
func TestMigrateTimestampColumnsCoverSchema(t *testing.T) {
	t.Parallel()
	// Locate the MySQL schema relative to this test file (we may run
	// from a temp-dir cwd under `go test`).
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// cwd is `<repo>/cmd`; the schema is at `<repo>/internal/store/mysql/schema.sql`.
	schemaPath := filepath.Join(repoRoot, "..", "internal", "store", "mysql", "schema.sql")
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}

	// Parse `CREATE TABLE ... (...)` blocks per migrationTables.name and
	// collect every column line that mentions DATETIME(6). Regex is fine
	// here: schema.sql is canonical workbuddy-owned DDL with a fixed shape.
	for _, mt := range migrationTables {
		block := tableBlock(string(raw), mt.name)
		if block == "" {
			t.Errorf("table %q: CREATE TABLE block not found in schema.sql", mt.name)
			continue
		}
		want := datetimeColumnsFromBlock(block)
		got := append([]string(nil), mt.timestampCols...)
		sort.Strings(want)
		sort.Strings(got)
		if !equalStringSlices(want, got) {
			t.Errorf("table %q: timestampCols drift\n  schema DATETIME cols: %v\n  migrate flag:         %v",
				mt.name, want, got)
		}
	}
}

// TestNewTimestampRewriter_SQLiteIdentity pins the SQLite-destination
// contract: a value written by internal/store on SQLite (RFC3339 with
// trailing 'Z') must come back unchanged after the parse-then-format
// round-trip. Otherwise a forced re-migrate would needlessly rewrite
// every DATETIME row and trip downstream lexicographic compares against
// pre-migrate cutoffs.
func TestNewTimestampRewriter_SQLiteIdentity(t *testing.T) {
	t.Parallel()
	rw := newTimestampRewriter("sqlite:///tmp/anywhere.db")
	in := "2026-04-18T11:30:45Z"
	got := rw(in)
	if s, ok := got.(string); !ok || s != in {
		t.Fatalf("SQLite identity: in=%q got=%v (%T), want unchanged", in, got, got)
	}
}

// TestNewTimestampRewriter_MySQLConversion pins the SQLite → MySQL fix:
// an RFC3339 input must come out as the MySQL space-form DATETIME(6)
// literal, with no trailing 'Z' and no 'T' separator. This is the exact
// bug class REQ-156 fixes; if the rewriter ever regresses to identity on
// a MySQL DSN the migration silently corrupts under STRICT_TRANS_TABLES.
func TestNewTimestampRewriter_MySQLConversion(t *testing.T) {
	t.Parallel()
	rw := newTimestampRewriter("mysql://user:pw@tcp(127.0.0.1:3306)/wb")
	in := "2026-04-18T11:30:45Z"
	got, ok := rw(in).(string)
	if !ok {
		t.Fatalf("MySQL conversion: rewriter returned %T, want string", rw(in))
	}
	if strings.ContainsAny(got, "TZ") {
		t.Fatalf("MySQL conversion: %q must not contain 'T' or 'Z'", got)
	}
	if !strings.HasPrefix(got, "2026-04-18 11:30:45") {
		t.Fatalf("MySQL conversion: got %q, want '2026-04-18 11:30:45...'", got)
	}
}

// TestNewTimestampRewriter_NilAndEmpty pins the NULL/empty passthrough.
// A NULL scan target must stay nil (DATETIME columns can be NULL in
// task_queue.acked_at and friends), and a non-timestamp string that
// ParseTimestamp rejects must be left as-is so the destination driver
// can produce a real error rather than have us silently re-encode.
func TestNewTimestampRewriter_NilAndEmpty(t *testing.T) {
	t.Parallel()
	rw := newTimestampRewriter("mysql://x/wb")
	if got := rw(nil); got != nil {
		t.Errorf("nil passthrough: got %v, want nil", got)
	}
	if got := rw(""); got != "" {
		t.Errorf("empty-string passthrough: got %v, want \"\"", got)
	}
	if got := rw("not a timestamp"); got != "not a timestamp" {
		t.Errorf("garbage passthrough: got %v, want %q", got, "not a timestamp")
	}
}

// TestNewTimestampRewriter_TimeValue covers the time.Time branch — a
// MySQL source with parseTime=true would surface DATETIME columns as
// time.Time, not as string. Today the migrator's source is SQLite, but
// the rewriter must keep working symmetrically so a future MySQL → X
// leg does not silently fall through.
func TestNewTimestampRewriter_TimeValue(t *testing.T) {
	t.Parallel()
	rw := newTimestampRewriter("mysql://x/wb")
	in := time.Date(2026, 4, 18, 11, 30, 45, 0, time.UTC)
	got, ok := rw(in).(string)
	if !ok {
		t.Fatalf("time.Time input: rewriter returned %T, want string", rw(in))
	}
	if !strings.HasPrefix(got, "2026-04-18 11:30:45") {
		t.Fatalf("time.Time → MySQL: got %q", got)
	}

	// Zero time.Time → nil (DATETIME NULL).
	if rw(time.Time{}) != nil {
		t.Errorf("zero time.Time should map to nil for nullable DATETIME columns")
	}
}

// TestDBMigrateDatetimeRoundTripSQLite seeds a SQLite source with an
// issue_claim row whose DATETIME columns receive concrete acquired_at /
// expires_at values via the production AcquireIssueClaim path (which
// already routes through dialect.FormatTimestamp). The migrate runs
// SQLite → SQLite, then a round-trip read via QueryIssueClaim must
// recover the same instants within microseconds.
//
// This is the SQLite-side proof that the rewriter is identity-preserving
// end-to-end; the MySQL side is covered by the mysql_integration test.
func TestDBMigrateDatetimeRoundTripSQLite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srcDSN := "sqlite://" + filepath.Join(dir, "src.db")
	dstDSN := "sqlite://" + filepath.Join(dir, "dst.db")

	src := mustOpen(t, srcDSN)
	// AcquireIssueClaim writes acquired_at + expires_at via the
	// dialect-routed formatter — exactly the shape the bug class
	// originates from.
	res, err := src.AcquireIssueClaim("owner/repo", 318, "worker-mig", time.Minute)
	if err != nil {
		t.Fatalf("seed AcquireIssueClaim: %v", err)
	}
	if res.ClaimToken == "" {
		t.Fatalf("expected non-empty claim token")
	}
	srcRec, err := src.QueryIssueClaim("owner/repo", 318)
	if err != nil || srcRec == nil {
		t.Fatalf("source QueryIssueClaim: rec=%+v err=%v", srcRec, err)
	}
	_ = src.Close()

	// Pre-create destination (store.New also runs createTables, which is
	// what the migrator expects from a "schema-ready dst").
	mustClose(t, mustOpen(t, dstDSN))

	var progress bytes.Buffer
	if err := runDBMigrate(context.Background(), dbMigrateOpts{
		from: srcDSN,
		to:   dstDSN,
	}, &progress); err != nil {
		t.Fatalf("runDBMigrate: %v\n%s", err, progress.String())
	}

	dst := mustOpen(t, dstDSN)
	defer func() { _ = dst.Close() }()
	dstRec, err := dst.QueryIssueClaim("owner/repo", 318)
	if err != nil {
		t.Fatalf("dst QueryIssueClaim: %v", err)
	}
	if dstRec == nil {
		t.Fatalf("issue_claim row missing on destination")
	}

	// Within microseconds (SQLite RFC3339 has second precision, MySQL
	// has microsecond precision; the tightest cross-engine bound that
	// SQLite-only data can satisfy is whole-second equality).
	if !dstRec.AcquiredAt.Truncate(time.Second).Equal(srcRec.AcquiredAt.Truncate(time.Second)) {
		t.Errorf("acquired_at drift: src=%v dst=%v",
			srcRec.AcquiredAt, dstRec.AcquiredAt)
	}
	if !dstRec.ExpiresAt.Truncate(time.Second).Equal(srcRec.ExpiresAt.Truncate(time.Second)) {
		t.Errorf("expires_at drift: src=%v dst=%v",
			srcRec.ExpiresAt, dstRec.ExpiresAt)
	}
	if dstRec.WorkerID != "worker-mig" {
		t.Errorf("worker_id lost in transit: %q", dstRec.WorkerID)
	}
}

// ---------------------------------------------------------------------------
// helpers for TestMigrateTimestampColumnsCoverSchema
// ---------------------------------------------------------------------------

var (
	// matches `<col> ... DATETIME(...)` on a single column line.
	reColumnLine = regexp.MustCompile(`^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s+[A-Z]`)
	reDatetime   = regexp.MustCompile(`(?i)\bDATETIME\b`)
)

// tableBlock returns the body between `CREATE TABLE IF NOT EXISTS <name> (`
// and the matching closing `);` for the named table, or "" if not found.
// Schema is canonical workbuddy-owned DDL so a simple paren counter
// suffices; we do not need a full SQL parser.
func tableBlock(schema, table string) string {
	marker := "CREATE TABLE IF NOT EXISTS " + table + " ("
	i := strings.Index(schema, marker)
	if i < 0 {
		return ""
	}
	rest := schema[i+len(marker):]
	depth := 1
	for j, r := range rest {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return rest[:j]
			}
		}
	}
	return ""
}

func datetimeColumnsFromBlock(block string) []string {
	var cols []string
	for _, line := range strings.Split(block, "\n") {
		if !reDatetime.MatchString(line) {
			continue
		}
		// A line like "    created_at  DATETIME(6) ..." has the column
		// name at the start. Skip constraint-bearing lines that begin
		// with PRIMARY/UNIQUE/KEY/CONSTRAINT.
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), "PRIMARY ") ||
			strings.HasPrefix(strings.ToUpper(trimmed), "UNIQUE ") ||
			strings.HasPrefix(strings.ToUpper(trimmed), "KEY ") ||
			strings.HasPrefix(strings.ToUpper(trimmed), "CONSTRAINT ") {
			continue
		}
		m := reColumnLine.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		cols = append(cols, m[1])
	}
	return cols
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
