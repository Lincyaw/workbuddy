// Tests for root_trace_id schema + write path + accessors (REQ-137 / #317).
//
// AC coverage:
//   - AC-1-1: legacy SQLite DB (no root_trace_id column) migrates cleanly
//     and pre-existing rows surface '' for root_trace_id.
//   - AC-1-2: a newly ingested issue has a non-empty 32-hex trace_id.
//   - AC-1-3: a PR ingested into issue_cache also has a non-empty
//     32-hex trace_id. The PR<->parent-issue linkage is not yet at the
//     schema layer (no parent_issue_num column on issue_cache), so this
//     test documents the "not-yet-linked" behaviour: PR trace_id is
//     fresh, distinct from its parent issue. Cross-linking will land
//     with the span-correlation work in #320.
//   - AC-1-4: accessor methods cover both issues and PRs.

package store

import (
	"database/sql"
	"path/filepath"
	"regexp"
	"testing"

	_ "modernc.org/sqlite"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestUpsertIssueCache_MintsRootTraceID(t *testing.T) {
	s := newTestStore(t)
	const repo = "owner/repo"
	const issueNum = 42

	if err := s.UpsertIssueCache(IssueCache{
		Repo: repo, IssueNum: issueNum, Labels: `["bug"]`, State: "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}

	tid, err := s.GetIssueRootTraceID(repo, issueNum)
	if err != nil {
		t.Fatalf("GetIssueRootTraceID: %v", err)
	}
	if !hex32.MatchString(tid) {
		t.Fatalf("expected 32-hex trace_id, got %q", tid)
	}

	// Subsequent upserts must NOT rotate the trace_id.
	if err := s.UpsertIssueCache(IssueCache{
		Repo: repo, IssueNum: issueNum, Labels: `["bug","priority"]`, State: "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache (update): %v", err)
	}
	tid2, err := s.GetIssueRootTraceID(repo, issueNum)
	if err != nil {
		t.Fatalf("GetIssueRootTraceID (after update): %v", err)
	}
	if tid2 != tid {
		t.Fatalf("root_trace_id should be stable across updates, got %q -> %q", tid, tid2)
	}
}

func TestUpsertIssueCache_PRWithoutParentMintsOwnTraceID(t *testing.T) {
	s := newTestStore(t)
	const repo = "owner/repo"
	// PR rows live in issue_cache too, discriminated by state "pr:*"
	// and using the PR number for issue_num. When ParentIssueNum is 0
	// (the PR branch did not encode an issue number, or the parent was
	// not sighted yet) the PR mints its own root_trace_id.
	if err := s.UpsertIssueCache(IssueCache{
		Repo: repo, IssueNum: 100, Labels: `["bug"]`, State: "open",
	}); err != nil {
		t.Fatalf("upsert parent issue: %v", err)
	}
	if err := s.UpsertIssueCache(IssueCache{
		Repo: repo, IssueNum: 200, State: "pr:open",
		// No ParentIssueNum — independent PR.
	}); err != nil {
		t.Fatalf("upsert PR: %v", err)
	}

	issueTID, err := s.GetIssueRootTraceID(repo, 100)
	if err != nil {
		t.Fatalf("GetIssueRootTraceID: %v", err)
	}
	prTID, err := s.GetPRRootTraceID(repo, 200)
	if err != nil {
		t.Fatalf("GetPRRootTraceID: %v", err)
	}
	if !hex32.MatchString(prTID) {
		t.Fatalf("PR root_trace_id not 32-hex: %q", prTID)
	}
	if !hex32.MatchString(issueTID) {
		t.Fatalf("issue root_trace_id not 32-hex: %q", issueTID)
	}
	// Without a parent link the two must be independent.
	if prTID == issueTID {
		t.Fatalf("unparented PR should mint its own trace_id, got shared %q", prTID)
	}
}

// REQ-138 (#320): when a PR is upserted with ParentIssueNum pointing at
// an already-sighted issue, the PR row inherits the parent's
// root_trace_id rather than minting a fresh one — this is what gives
// the full issue+PR lifecycle a single trace_id.
func TestUpsertIssueCache_PRInheritsParentTraceID(t *testing.T) {
	s := newTestStore(t)
	const repo = "owner/repo"

	if err := s.UpsertIssueCache(IssueCache{
		Repo: repo, IssueNum: 50, Labels: `["bug"]`, State: "open",
	}); err != nil {
		t.Fatalf("upsert parent issue: %v", err)
	}
	parentTID, err := s.GetIssueRootTraceID(repo, 50)
	if err != nil || parentTID == "" {
		t.Fatalf("parent trace_id missing: %v / %q", err, parentTID)
	}

	if err := s.UpsertIssueCache(IssueCache{
		Repo: repo, IssueNum: 150, State: "pr:open", ParentIssueNum: 50,
	}); err != nil {
		t.Fatalf("upsert PR: %v", err)
	}
	prTID, err := s.GetPRRootTraceID(repo, 150)
	if err != nil {
		t.Fatalf("GetPRRootTraceID: %v", err)
	}
	if prTID != parentTID {
		t.Fatalf("PR should inherit parent trace_id %q, got %q", parentTID, prTID)
	}

	// Updating the PR row must NOT rotate the trace_id.
	if err := s.UpsertIssueCache(IssueCache{
		Repo: repo, IssueNum: 150, State: "pr:closed", ParentIssueNum: 50,
	}); err != nil {
		t.Fatalf("upsert PR (update): %v", err)
	}
	prTID2, _ := s.GetPRRootTraceID(repo, 150)
	if prTID2 != parentTID {
		t.Fatalf("PR trace_id rotated on update: %q -> %q", parentTID, prTID2)
	}
}

// Fallback path: a PR row whose own root_trace_id is empty (e.g. row
// minted before the parent was sighted, then parent_issue_num filled in
// later via an upsert) returns the parent's trace_id when queried.
func TestGetPRRootTraceID_FallsBackThroughParentLink(t *testing.T) {
	s := newTestStore(t)
	const repo = "owner/repo"

	// Hand-craft a PR row with empty trace_id but a parent link, to
	// simulate a row inherited from a legacy migration.
	if err := s.UpsertIssueCache(IssueCache{
		Repo: repo, IssueNum: 9, Labels: `["bug"]`, State: "open",
	}); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}
	parentTID, _ := s.GetIssueRootTraceID(repo, 9)
	if _, err := s.Exec(
		`INSERT INTO issue_cache (repo, issue_num, labels, body, state, root_trace_id, parent_issue_num)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		repo, 19, "", "", "pr:open", "", 9,
	); err != nil {
		t.Fatalf("hand-insert legacy PR row: %v", err)
	}
	got, err := s.GetPRRootTraceID(repo, 19)
	if err != nil {
		t.Fatalf("GetPRRootTraceID: %v", err)
	}
	if got != parentTID {
		t.Fatalf("expected fallback to parent trace_id %q, got %q", parentTID, got)
	}
}

func TestRootTraceID_AccessorsReturnEmptyWhenMissing(t *testing.T) {
	s := newTestStore(t)
	tid, err := s.GetIssueRootTraceID("owner/repo", 999)
	if err != nil {
		t.Fatalf("GetIssueRootTraceID on missing: %v", err)
	}
	if tid != "" {
		t.Fatalf("expected empty trace_id for missing row, got %q", tid)
	}
	tid, err = s.GetPRRootTraceID("owner/repo", 999)
	if err != nil {
		t.Fatalf("GetPRRootTraceID on missing: %v", err)
	}
	if tid != "" {
		t.Fatalf("expected empty trace_id for missing PR row, got %q", tid)
	}
}

// TestMigration_LegacyDBGainsRootTraceIDColumn simulates opening a
// SQLite file that predates this PR — issue_cache exists with the old
// columns but no root_trace_id — and verifies the ALTER TABLE adds the
// column non-destructively and pre-existing rows surface '' via the
// accessor (AC-1-1).
func TestMigration_LegacyDBGainsRootTraceIDColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// Phase 1: build a legacy schema by hand, insert a row.
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`CREATE TABLE issue_cache (
		repo TEXT NOT NULL,
		issue_num INTEGER NOT NULL,
		labels TEXT,
		body TEXT NOT NULL DEFAULT '',
		state TEXT,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (repo, issue_num)
	)`); err != nil {
		t.Fatalf("create legacy issue_cache: %v", err)
	}
	if _, err := legacyDB.Exec(
		`INSERT INTO issue_cache (repo, issue_num, labels, body, state) VALUES (?, ?, ?, ?, ?)`,
		"owner/repo", 7, `["bug"]`, "legacy body", "open",
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// Phase 2: re-open via NewStore — schema migration must add the
	// new column without dropping the legacy row.
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore on legacy db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Pre-existing row must still be visible.
	ic, err := s.QueryIssueCache("owner/repo", 7)
	if err != nil {
		t.Fatalf("QueryIssueCache: %v", err)
	}
	if ic == nil {
		t.Fatal("legacy row was lost during migration")
	}
	if ic.Body != "legacy body" {
		t.Fatalf("legacy row mutated: body=%q", ic.Body)
	}

	// Pre-existing rows get '' for root_trace_id (default), as spec'd.
	tid, err := s.GetIssueRootTraceID("owner/repo", 7)
	if err != nil {
		t.Fatalf("GetIssueRootTraceID on legacy row: %v", err)
	}
	if tid != "" {
		t.Fatalf("pre-existing row should default to empty trace_id, got %q", tid)
	}

	// And new upserts on the migrated DB DO mint trace_ids.
	if err := s.UpsertIssueCache(IssueCache{
		Repo: "owner/repo", IssueNum: 8, State: "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache after migration: %v", err)
	}
	tid, err = s.GetIssueRootTraceID("owner/repo", 8)
	if err != nil {
		t.Fatalf("GetIssueRootTraceID on new row: %v", err)
	}
	if !hex32.MatchString(tid) {
		t.Fatalf("new row trace_id not 32-hex: %q", tid)
	}
}
