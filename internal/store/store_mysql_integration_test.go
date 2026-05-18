//go:build mysql_integration

// MySQL integration tests for the workbuddy store. Compiled in only when
// the `mysql_integration` build tag is set; the default `go test ./...`
// run continues to exercise the SQLite path only.
//
// To run:
//
//	export WORKBUDDY_MYSQL_TEST_DSN='mysql://root:secret@tcp(127.0.0.1:3307)/workbuddy?parseTime=true&loc=UTC&multiStatements=true'
//	go test -tags mysql_integration ./internal/store/... -count=1
//
// See internal/store/mysql/README.md for a docker-compose snippet that
// stands up a disposable MySQL.

package store

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const mysqlDSNEnv = "WORKBUDDY_MYSQL_TEST_DSN"

// newTestMySQLStore opens (or reuses) the MySQL test database, drops
// every workbuddy table so each test starts from a clean slate, and
// returns the Store.
func newTestMySQLStore(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv(mysqlDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set — skipping MySQL integration test. See internal/store/mysql/README.md.", mysqlDSNEnv)
	}
	if !strings.HasPrefix(dsn, "mysql://") {
		dsn = "mysql://" + dsn
	}
	driverDSN := strings.TrimPrefix(dsn, "mysql://")

	rawDB, err := sql.Open("mysql", driverDSN)
	if err != nil {
		t.Fatalf("open mysql for reset: %v", err)
	}
	defer func() { _ = rawDB.Close() }()
	if err := rawDB.Ping(); err != nil {
		t.Fatalf("ping mysql: %v (DSN %q)", err, driverDSN)
	}
	// Drop tables in FK-aware order. The schema has one FK
	// (workflow_transitions → workflow_instances); SET FOREIGN_KEY_CHECKS
	// off is simpler and more robust against future additions.
	if _, err := rawDB.Exec("SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatalf("disable fk checks: %v", err)
	}
	for _, tbl := range []string{
		"events", "task_queue", "workers", "repo_registrations",
		"transition_counts", "workflow_instances", "workflow_transitions",
		"issue_cache", "agent_sessions", "sessions", "session_routes",
		"issue_dependencies", "issue_dependency_state", "issue_claim",
		"issue_pipeline_hazards", "issue_cycle_state",
	} {
		if _, err := rawDB.Exec("DROP TABLE IF EXISTS " + tbl); err != nil {
			t.Fatalf("drop table %s: %v", tbl, err)
		}
	}
	if _, err := rawDB.Exec("SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		t.Fatalf("re-enable fk checks: %v", err)
	}

	s, err := New(dsn)
	if err != nil {
		t.Fatalf("New mysql: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestMySQLBootstrap verifies that connecting to a clean MySQL with the
// `mysql://` scheme creates the schema and exposes a working Store.
func TestMySQLBootstrap(t *testing.T) {
	s := newTestMySQLStore(t)
	if err := s.InsertWorker(WorkerRecord{
		ID:       "test-worker",
		Repo:     "owner/repo",
		Roles:    `["dev"]`,
		Hostname: "localhost",
		Status:   "online",
	}); err != nil {
		t.Fatalf("insert worker: %v", err)
	}
	got, err := s.GetWorker("test-worker")
	if err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got == nil || got.ID != "test-worker" {
		t.Fatalf("worker round-trip failed: got %+v", got)
	}
}

// TestMySQLEventLifecycle exercises the events table — covers
// InsertEvent (LastInsertId path), QueryEvents, CountEventsByRepoType.
func TestMySQLEventLifecycle(t *testing.T) {
	s := newTestMySQLStore(t)
	id, err := s.InsertEvent(Event{
		Type: "dispatch", Repo: "owner/repo", IssueNum: 1, Payload: `{"k":"v"}`,
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected non-zero LastInsertId, got 0")
	}
	evs, err := s.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != "dispatch" {
		t.Fatalf("unexpected events: %+v", evs)
	}
	agg, err := s.CountEventsByRepoType()
	if err != nil {
		t.Fatalf("CountEventsByRepoType: %v", err)
	}
	if len(agg) != 1 || agg[0].Count != 1 {
		t.Fatalf("unexpected aggregate: %+v", agg)
	}
}

// TestMySQLTaskClaimLifecycle exercises the full claim lifecycle: insert,
// claim with a lease (datetime('now', ?) rewriting), heartbeat, complete.
// This is the highest-traffic surface in the dialect layer.
func TestMySQLTaskClaimLifecycle(t *testing.T) {
	s := newTestMySQLStore(t)
	if err := s.InsertTask(TaskRecord{
		ID: "t1", Repo: "owner/repo", IssueNum: 1, AgentName: "dev-agent",
		Role: "dev", Runtime: "claude-code",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	got, err := s.ClaimNextTask("worker-1", []string{"dev"}, []string{"owner/repo"}, "claude-code", "ct-1", 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextTask: %v", err)
	}
	if got == nil {
		t.Fatalf("expected task, got nil")
	}
	if err := s.AckTask(got.ID, "worker-1", 30*time.Second); err != nil {
		t.Fatalf("AckTask: %v", err)
	}
	if err := s.HeartbeatTask(got.ID, "worker-1", 30*time.Second); err != nil {
		t.Fatalf("HeartbeatTask: %v", err)
	}
	if err := s.CompleteTask(got.ID, "worker-1", 0, "[]"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	final, err := s.GetTask(got.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if final.Status != TaskStatusCompleted {
		t.Fatalf("expected completed, got %s", final.Status)
	}
}

// TestMySQLTransitionCountsRETURNING checks that the RETURNING-emulation
// path returns the post-increment count correctly across multiple calls.
func TestMySQLTransitionCountsRETURNING(t *testing.T) {
	s := newTestMySQLStore(t)
	for i := 1; i <= 3; i++ {
		n, err := s.IncrementTransition("owner/repo", 7, "dev", "review")
		if err != nil {
			t.Fatalf("IncrementTransition #%d: %v", i, err)
		}
		if n != i {
			t.Fatalf("IncrementTransition #%d: want %d, got %d", i, i, n)
		}
	}
}

// TestMySQLIssueCycleStateRETURNING covers the two cycle-counter
// RETURNING emulations.
func TestMySQLIssueCycleStateRETURNING(t *testing.T) {
	s := newTestMySQLStore(t)
	for i := 1; i <= 2; i++ {
		n, err := s.IncrementDevReviewCycleCount("owner/repo", 1)
		if err != nil {
			t.Fatalf("IncrementDevReviewCycleCount: %v", err)
		}
		if n != i {
			t.Fatalf("dev_review_cycle_count want %d, got %d", i, n)
		}
	}
	for i := 1; i <= 2; i++ {
		n, err := s.IncrementSynthCycleCount("owner/repo", 1)
		if err != nil {
			t.Fatalf("IncrementSynthCycleCount: %v", err)
		}
		if n != i {
			t.Fatalf("synth_cycle_count want %d, got %d", i, n)
		}
	}
}

// TestMySQLIssueClaimRoundtrip exercises the issue-claim upsert path that
// uses INSERT … ON CONFLICT … DO UPDATE SET … excluded.X (rewritten to
// ON DUPLICATE KEY UPDATE … VALUES(X)).
func TestMySQLIssueClaimRoundtrip(t *testing.T) {
	s := newTestMySQLStore(t)
	res, err := s.AcquireIssueClaim("owner/repo", 1, "worker-1", 1*time.Minute)
	if err != nil {
		t.Fatalf("AcquireIssueClaim: %v", err)
	}
	if res.ClaimToken == "" {
		t.Fatalf("expected token, got empty")
	}
	rec, err := s.QueryIssueClaim("owner/repo", 1)
	if err != nil {
		t.Fatalf("QueryIssueClaim: %v", err)
	}
	if rec == nil || rec.WorkerID != "worker-1" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	ok, err := s.ReleaseIssueClaim("owner/repo", 1, "worker-1", res.ClaimToken)
	if err != nil || !ok {
		t.Fatalf("ReleaseIssueClaim: ok=%v err=%v", ok, err)
	}
}

// TestMySQLIssueClaimStrictSqlMode pins the REQ-154 contract under strict
// sql_mode: AcquireIssueClaim (fresh + self-extend + overwrite-expired) and
// RefreshIssueClaim must succeed against MySQL when the connection's
// sql_mode includes STRICT_TRANS_TABLES — the default on MySQL 8 production
// servers. The original bug class wrote RFC3339 ("2026-04-18T11:30:00Z")
// into DATETIME(6) columns; strict mode rejects the literal with "Incorrect
// datetime value" so the INSERT fails. The dialect-routed FormatTimestamp
// helper emits the MySQL-native space form, which is accepted unconditionally.
//
// The test sets sql_mode at the session level. Some managed MySQL offerings
// override sql_mode at the server level and ignore session settings; in
// those environments the SET succeeds but the actual mode on the connection
// may differ. We read the effective sql_mode back and skip with a clear
// message if STRICT_TRANS_TABLES cannot be enabled, so the test is a strict
// upper bound: a passing run guarantees strict-mode correctness; a skip is
// an environmental limitation, not a silent FALSE PASS.
func TestMySQLIssueClaimStrictSqlMode(t *testing.T) {
	// Pin sql_mode at the DSN level: the go-sql-driver/mysql `sql_mode`
	// parameter is applied on every connection the pool opens, so we
	// exercise the same surface a production strict-mode deployment hits
	// regardless of which pool entry a given query lands on. Strict mode
	// rejects DATETIME(6) literals containing `T`/`Z`; the dialect-routed
	// FormatTimestamp helper emits the MySQL-native space form, which is
	// accepted unconditionally.
	dsn := os.Getenv(mysqlDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set — skipping strict-mode integration test", mysqlDSNEnv)
	}
	if !strings.HasPrefix(dsn, "mysql://") {
		dsn = "mysql://" + dsn
	}
	driverDSN := strings.TrimPrefix(dsn, "mysql://")
	sep := "?"
	if strings.Contains(driverDSN, "?") {
		sep = "&"
	}
	strictDSN := "mysql://" + driverDSN + sep + "sql_mode=%27STRICT_TRANS_TABLES%2CNO_ZERO_IN_DATE%2CNO_ZERO_DATE%27"

	strictStore, err := New(strictDSN)
	if err != nil {
		t.Fatalf("open strict-mode store: %v", err)
	}
	t.Cleanup(func() { _ = strictStore.Close() })

	// Assert strict mode is actually in force on the connection we're
	// about to write through. Managed MySQL offerings sometimes override
	// sql_mode at the server level and ignore the DSN-level pin; in that
	// case skip with a clear message rather than producing a false PASS
	// when the writes happen to succeed for unrelated reasons.
	var mode string
	if err := strictStore.QueryRow("SELECT @@sql_mode").Scan(&mode); err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}
	if !strings.Contains(mode, "STRICT_TRANS_TABLES") {
		t.Skipf("server effective sql_mode=%q does not include STRICT_TRANS_TABLES; managed MySQL may override the DSN pin", mode)
	}
	t.Logf("confirmed strict mode active: sql_mode=%q", mode)

	// Fresh acquire (INSERT path).
	res, err := strictStore.AcquireIssueClaim("owner/repo", 100, "worker-strict", 1*time.Minute)
	if err != nil {
		t.Fatalf("AcquireIssueClaim under strict sql_mode: %v", err)
	}
	if res.ClaimToken == "" {
		t.Fatalf("expected non-empty token")
	}

	// Self-extend (UPDATE-in-place path).
	res2, err := strictStore.AcquireIssueClaim("owner/repo", 100, "worker-strict", 2*time.Minute)
	if err != nil {
		t.Fatalf("AcquireIssueClaim self-extend under strict sql_mode: %v", err)
	}
	if !res2.Extended {
		t.Fatalf("expected Extended=true on self-extend, got %+v", res2)
	}
	if res2.ClaimToken != res.ClaimToken {
		t.Fatalf("self-extend changed the token: prior=%q now=%q", res.ClaimToken, res2.ClaimToken)
	}

	// Refresh (RefreshIssueClaim path — independent UPDATE with the
	// dialect-normalised WHERE cutoff).
	ok, err := strictStore.RefreshIssueClaim("owner/repo", 100, "worker-strict", res.ClaimToken, 3*time.Minute)
	if err != nil {
		t.Fatalf("RefreshIssueClaim under strict sql_mode: %v", err)
	}
	if !ok {
		t.Fatalf("expected RefreshIssueClaim to update the row")
	}

	// Read-back must still parse cleanly.
	rec, err := strictStore.QueryIssueClaim("owner/repo", 100)
	if err != nil {
		t.Fatalf("QueryIssueClaim under strict sql_mode: %v", err)
	}
	if rec == nil || rec.WorkerID != "worker-strict" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.AcquiredAt.IsZero() || rec.ExpiresAt.IsZero() {
		t.Fatalf("expected non-zero timestamps, got acquired_at=%v expires_at=%v",
			rec.AcquiredAt, rec.ExpiresAt)
	}
	if !rec.ExpiresAt.After(rec.AcquiredAt) {
		t.Fatalf("expires_at %v must be after acquired_at %v",
			rec.ExpiresAt, rec.AcquiredAt)
	}
}

// TestMySQLIssueCacheUpsert exercises the second high-volume upsert path
// (INSERT … ON CONFLICT (repo, issue_num) DO UPDATE SET …).
func TestMySQLIssueCacheUpsert(t *testing.T) {
	s := newTestMySQLStore(t)
	for i := 0; i < 3; i++ {
		if err := s.UpsertIssueCache(IssueCache{
			Repo: "owner/repo", IssueNum: 1,
			Labels: fmt.Sprintf(`["v%d"]`, i),
			Body:   fmt.Sprintf("body %d", i),
			State:  "open",
		}); err != nil {
			t.Fatalf("UpsertIssueCache #%d: %v", i, err)
		}
	}
	got, err := s.QueryIssueCache("owner/repo", 1)
	if err != nil || got == nil {
		t.Fatalf("QueryIssueCache: %+v err=%v", got, err)
	}
	if got.Labels != `["v2"]` {
		t.Fatalf("expected latest labels, got %q", got.Labels)
	}
}
