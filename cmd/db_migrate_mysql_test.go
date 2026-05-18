//go:build mysql_integration

// Integration variant of TestDBMigrateHappyPath that targets a real MySQL
// destination. Gated by the mysql_integration build tag and skipped unless
// WORKBUDDY_MYSQL_TEST_DSN is set, mirroring the convention in
// internal/store/store_mysql_integration_test.go.
//
// Example:
//
//	WORKBUDDY_MYSQL_TEST_DSN='mysql://root:root@tcp(127.0.0.1:3306)/workbuddy_migrate_test' \
//	  go test -tags mysql_integration ./cmd/... -run TestDBMigrateToMySQL -count=1
//
// See internal/store/mysql/README.md for the docker-compose snippet that
// stands up the matching MySQL container.
package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestDBMigrateToMySQL(t *testing.T) {
	dsn := os.Getenv("WORKBUDDY_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("WORKBUDDY_MYSQL_TEST_DSN not set; skipping real-MySQL migrate test")
	}

	dir := t.TempDir()
	srcDSN := "sqlite://" + filepath.Join(dir, "src.db")

	src := mustOpen(t, srcDSN)
	seedRepresentative(t, src)
	_ = src.Close()

	// Wipe destination so re-runs of this test stay deterministic.
	dst := mustOpen(t, dsn)
	if err := wipeDestination(dst); err != nil {
		t.Fatalf("pre-wipe destination: %v", err)
	}
	_ = dst.Close()

	var progress bytes.Buffer
	if err := runDBMigrate(context.Background(), dbMigrateOpts{
		from: srcDSN,
		to:   dsn,
	}, &progress); err != nil {
		t.Fatalf("runDBMigrate: %v\n%s", err, progress.String())
	}

	dst = mustOpen(t, dsn)
	defer func() { _ = dst.Close() }()

	got, err := dst.QueryIssueCache("owner/repo", 318)
	if err != nil {
		t.Fatalf("QueryIssueCache: %v", err)
	}
	if got == nil {
		t.Fatalf("issue_cache row missing on MySQL after migrate")
	}
	_ = store.IssueCache{} // silence unused-import warning if struct ref ever drops
}

// TestDBMigrateToMySQLStrictSqlMode pins REQ-156: SQLite → MySQL migration
// must succeed when the destination connection's sql_mode includes
// `STRICT_TRANS_TABLES` (the default on production MySQL 8). Pre-fix the
// migrate path bound the SQLite RFC3339 timestamp string verbatim into
// the MySQL DATETIME(6) INSERT and the server rejected it with "Incorrect
// datetime value: '...Z'"; post-fix the timestamp rewriter converts each
// flagged column to the MySQL-native space-form literal.
//
// The DSN-level `sql_mode` parameter is applied by the
// go-sql-driver/mysql connector on every pooled connection, so every
// INSERT in the migration crosses the strict-mode bar regardless of
// which connection it lands on. This mirrors the pattern in
// internal/store/store_mysql_integration_test.go::TestMySQLIssueClaimStrictSqlMode.
//
// Managed MySQL offerings sometimes override sql_mode at the server
// level and ignore the DSN pin; we read the effective mode back after
// connecting and SKIP with a clear message if STRICT_TRANS_TABLES did
// not stick — a passing run is a strict upper bound, a skip is an
// environmental limitation, never a silent false PASS.
func TestDBMigrateToMySQLStrictSqlMode(t *testing.T) {
	rawDSN := os.Getenv("WORKBUDDY_MYSQL_TEST_DSN")
	if rawDSN == "" {
		t.Skip("WORKBUDDY_MYSQL_TEST_DSN not set; skipping strict-mode migrate test")
	}
	// Pin sql_mode at the DSN level.
	if !strings.HasPrefix(rawDSN, "mysql://") {
		rawDSN = "mysql://" + rawDSN
	}
	driverDSN := strings.TrimPrefix(rawDSN, "mysql://")
	sep := "?"
	if strings.Contains(driverDSN, "?") {
		sep = "&"
	}
	strictDSN := "mysql://" + driverDSN + sep + "sql_mode=%27STRICT_TRANS_TABLES%2CNO_ZERO_IN_DATE%2CNO_ZERO_DATE%27"

	// Verify strict mode is in force on a probe connection before we
	// invest in seeding + running the migration.
	probe, err := store.New(strictDSN)
	if err != nil {
		t.Fatalf("open strict-mode store: %v", err)
	}
	var mode string
	if err := probe.QueryRow("SELECT @@sql_mode").Scan(&mode); err != nil {
		_ = probe.Close()
		t.Fatalf("read sql_mode: %v", err)
	}
	if !strings.Contains(mode, "STRICT_TRANS_TABLES") {
		_ = probe.Close()
		t.Skipf("server effective sql_mode=%q does not include STRICT_TRANS_TABLES; managed MySQL may override the DSN pin", mode)
	}
	t.Logf("confirmed strict mode active: sql_mode=%q", mode)
	// Wipe destination so the test is deterministic across re-runs.
	if err := wipeDestination(probe); err != nil {
		_ = probe.Close()
		t.Fatalf("pre-wipe destination: %v", err)
	}
	_ = probe.Close()

	// Build source with concrete, known-instant DATETIME values so we
	// can assert round-trip parity after the migration.
	dir := t.TempDir()
	srcDSN := "sqlite://" + filepath.Join(dir, "src.db")
	src := mustOpen(t, srcDSN)

	// issue_claim has two NOT NULL DATETIME(6) columns and is the
	// canonical bug-class surface: AcquireIssueClaim writes both
	// acquired_at and expires_at through the dialect-routed formatter.
	if _, err := src.AcquireIssueClaim("owner/repo", 156, "worker-strict-mig", 30*time.Minute); err != nil {
		t.Fatalf("seed AcquireIssueClaim: %v", err)
	}
	srcRec, err := src.QueryIssueClaim("owner/repo", 156)
	if err != nil || srcRec == nil {
		t.Fatalf("source QueryIssueClaim: rec=%+v err=%v", srcRec, err)
	}
	// Also seed a representative row in events (DATETIME ts) so we
	// exercise more than one table.
	if _, err := src.InsertEvent(store.Event{
		Type: "migrate.test", Repo: "owner/repo", IssueNum: 156,
		Payload: `{"strict":"yes"}`,
	}); err != nil {
		t.Fatalf("seed InsertEvent: %v", err)
	}
	_ = src.Close()

	var progress bytes.Buffer
	if err := runDBMigrate(context.Background(), dbMigrateOpts{
		from: srcDSN,
		to:   strictDSN,
	}, &progress); err != nil {
		t.Fatalf("runDBMigrate strict-mode: %v\n%s", err, progress.String())
	}

	dst := mustOpen(t, strictDSN)
	defer func() { _ = dst.Close() }()

	dstRec, err := dst.QueryIssueClaim("owner/repo", 156)
	if err != nil {
		t.Fatalf("dst QueryIssueClaim: %v", err)
	}
	if dstRec == nil {
		t.Fatalf("issue_claim row missing on strict MySQL after migrate")
	}
	if dstRec.WorkerID != "worker-strict-mig" {
		t.Errorf("worker_id lost: %q", dstRec.WorkerID)
	}
	// Microsecond bound: MySQL DATETIME(6) preserves microseconds; the
	// SQLite source writes RFC3339 (second precision). Compare on the
	// tighter common granularity — whole seconds — to validate parity.
	if diff := dstRec.AcquiredAt.Sub(srcRec.AcquiredAt); diff < -time.Second || diff > time.Second {
		t.Errorf("acquired_at drift > 1s: src=%v dst=%v", srcRec.AcquiredAt, dstRec.AcquiredAt)
	}
	if diff := dstRec.ExpiresAt.Sub(srcRec.ExpiresAt); diff < -time.Second || diff > time.Second {
		t.Errorf("expires_at drift > 1s: src=%v dst=%v", srcRec.ExpiresAt, dstRec.ExpiresAt)
	}

	// Tighter: microsecond bound. SQLite RFC3339 truncates fractional
	// seconds, so post-parse the time is whole-second; the destination
	// re-formats from that same parsed instant via FormatTimestamp, so
	// the destination row must be exactly equal to the (already
	// second-truncated) source instant — drift here would indicate a
	// real bug in the rewriter or in QueryIssueClaim's read-side
	// scan.
	if !dstRec.AcquiredAt.Equal(srcRec.AcquiredAt.Truncate(time.Second)) {
		// The source value itself was already second-precision (RFC3339
		// emit truncates), so QueryIssueClaim returns the truncated
		// instant on both sides. Use Equal not == because *time.Time
		// values carry zone info.
		t.Errorf("acquired_at not bit-equal at second precision: src=%v dst=%v",
			srcRec.AcquiredAt.Truncate(time.Second), dstRec.AcquiredAt)
	}
}
