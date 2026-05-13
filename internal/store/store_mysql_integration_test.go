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
