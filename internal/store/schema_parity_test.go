package store

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestSchemaParity guards the requirement that the SQLite create-table DDL
// embedded in `dbStore.createTables` (plus its `ALTER TABLE ... ADD
// COLUMN` migrations) stays feature-parallel with the MySQL schema in
// `internal/store/mysql/schema.sql`. The two backends are reachable via
// the same `Store` interface; if they drift at the table/column level the
// dialect rewrite layer cannot save us — the columns we read or write
// simply will not exist on one of the backends.
//
// What this test verifies, table by table:
//
//   - The set of table names is identical.
//   - The set of column names per table is identical.
//
// What it intentionally does NOT verify:
//
//   - Type names (SQLite TEXT vs MySQL VARCHAR(N) vs LONGTEXT). These
//     diverge by design; only the *Go* types they decode into matter and
//     those are covered by the regular store tests.
//   - Index/foreign-key shapes. MySQL needs key length prefixes for some
//     TEXT/VARCHAR indexes and InnoDB-specific ENGINE clauses that
//     SQLite doesn't accept. The functional behaviour they enable (e.g.
//     PRIMARY KEY on (repo, issue_num)) is exercised by the normal
//     tests.
//
// When you add a column, add it on both sides and this test will pass.
// When you drift, this test will fail and tell you which table/column
// is missing where. See issue #316.
func TestSchemaParity(t *testing.T) {
	t.Parallel()

	sqliteTables := parseSQLiteSchemaForTest(t, sqliteSchemaFixture())
	mysqlTables := parseMySQLSchemaForTest(t, mysqlSchemaSQL)

	if diff := tableNameDiff(sqliteTables, mysqlTables); diff != "" {
		t.Fatalf("table set drift between SQLite and MySQL backends:\n%s", diff)
	}

	for name, sqliteCols := range sqliteTables {
		mysqlCols := mysqlTables[name]
		if diff := columnNameDiff(sqliteCols, mysqlCols); diff != "" {
			t.Errorf("table %q: column drift between SQLite and MySQL:\n%s", name, diff)
		}
	}
}

// sqliteSchemaFixture returns the SQL fed to dbStore.createTables. We
// duplicate it here so the parity test can scan the canonical column
// list without standing up a live store; the same DDL is exercised by
// the SQLite open path so any drift between this fixture and
// createTables surfaces immediately as a TestParseSQLiteSchemaSelfCheck
// failure (below).
//
// Migrations applied by ALTER TABLE are appended at the end so columns
// added incrementally on SQLite are accounted for.
func sqliteSchemaFixture() string {
	return `
CREATE TABLE events (id, ts, type, repo, issue_num, payload);
CREATE TABLE task_queue (id, repo, issue_num, agent_name, role, runtime, workflow, state, worker_id, claim_token, status, lease_expires_at, acked_at, heartbeat_at, completed_at, exit_code, session_refs, rollout_index, rollouts_total, rollout_group_id, supervisor_agent_id, created_at, updated_at);
CREATE TABLE workers (id, repo, repos_json, roles, runtime, hostname, mgmt_base_url, audit_url, tunnel, status, token_kid, token_hash, token_revoked_at, last_heartbeat, registered_at);
CREATE TABLE repo_registrations (repo, environment, status, config_json, registered_at, updated_at);
CREATE TABLE transition_counts (repo, issue_num, from_state, to_state, count);
CREATE TABLE workflow_instances (id, workflow_name, repo, issue_num, current_state, created_at, updated_at);
CREATE TABLE workflow_transitions (id, workflow_instance_id, from_state, to_state, trigger_agent, created_at);
CREATE TABLE issue_cache (repo, issue_num, labels, body, state, root_trace_id, updated_at);
CREATE TABLE agent_sessions (id, session_id, task_id, repo, issue_num, agent_name, summary, raw_path, created_at);
CREATE TABLE sessions (id, session_id, task_id, repo, issue_num, agent_name, runtime, worker_id, attempt, status, dir, stdout_path, stderr_path, tool_calls_path, metadata_path, summary, raw_path, created_at, closed_at);
CREATE TABLE session_routes (session_id, worker_id, repo, issue_num, created_at);
CREATE TABLE issue_dependencies (repo, issue_num, depends_on_repo, depends_on_issue_num, source_hash, status);
CREATE TABLE issue_dependency_state (repo, issue_num, verdict, resume_label, blocked_reason_hash, override_active, graph_version, last_reaction_blocked, last_evaluated_at);
CREATE TABLE issue_claim (repo, issue_num, worker_id, claim_token, acquired_at, expires_at);
CREATE TABLE issue_pipeline_hazards (repo, issue_num, kind, fingerprint, detected_at);
CREATE TABLE issue_cycle_state (repo, issue_num, dev_review_cycle_count, synth_cycle_count, first_dispatch_at, cap_hit_at, synth_cap_hit_at, updated_at);
`
}

// TestParseSQLiteSchemaSelfCheck verifies that the fixture used by
// TestSchemaParity above stays in sync with the actual SQLite DDL run by
// dbStore.createTables, by opening a fresh in-memory SQLite store and
// reading PRAGMA table_info for every table. If a developer adds a
// column to createTables and forgets the fixture, this test fails first
// with a clear diff.
func TestParseSQLiteSchemaSelfCheck(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	live := liveSQLiteTablesForTest(t, s.(*dbStore))
	fixture := parseSQLiteSchemaForTest(t, sqliteSchemaFixture())
	if diff := tableNameDiff(fixture, live); diff != "" {
		t.Fatalf("sqliteSchemaFixture is out of date vs the real SQLite DDL:\n%s", diff)
	}
	for name, fixtureCols := range fixture {
		if diff := columnNameDiff(fixtureCols, live[name]); diff != "" {
			t.Errorf("table %q: sqliteSchemaFixture drifted from createTables:\n%s", name, diff)
		}
	}
}

// liveSQLiteTablesForTest dumps the columns of every user table on the
// given SQLite store. PRAGMA table_info is used directly via the store's
// Query method.
func liveSQLiteTablesForTest(t *testing.T, s *dbStore) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	rows, err := s.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, n)
	}
	for _, n := range names {
		colRows, err := s.Query(`PRAGMA table_info(` + n + `)`)
		if err != nil {
			t.Fatalf("table_info(%s): %v", n, err)
		}
		var cols []string
		for colRows.Next() {
			var (
				cid     int
				colName string
				colType string
				notnull int
				dflt    any
				pk      int
			)
			if err := colRows.Scan(&cid, &colName, &colType, &notnull, &dflt, &pk); err != nil {
				_ = colRows.Close()
				t.Fatalf("scan column: %v", err)
			}
			cols = append(cols, colName)
		}
		_ = colRows.Close()
		out[n] = cols
	}
	return out
}

// parseSQLiteSchemaForTest extracts {tableName -> columnNames} from a
// SQLite CREATE TABLE script. The parser is intentionally lazy: it
// captures the body inside the first matching parenthesis pair and
// splits on commas. Good enough for the limited DDL shapes we use.
func parseSQLiteSchemaForTest(t *testing.T, script string) map[string][]string {
	t.Helper()
	return parseSchemaForTest(t, script, sqliteSchemaTableRe)
}

func parseMySQLSchemaForTest(t *testing.T, script string) map[string][]string {
	t.Helper()
	// Strip the inline comments before splitting so the regex matches
	// cleanly.
	out := strings.Builder{}
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return parseSchemaForTest(t, out.String(), mysqlSchemaTableRe)
}

var (
	sqliteSchemaTableRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\(([^;]*?)\)\s*(?:WITHOUT\s+ROWID\s*)?;`)
	mysqlSchemaTableRe  = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\((.*?)\)\s*ENGINE=`)
)

func parseSchemaForTest(t *testing.T, script string, re *regexp.Regexp) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, m := range re.FindAllStringSubmatch(script, -1) {
		name := strings.ToLower(m[1])
		body := m[2]
		cols := parseColumnsFromBodyForTest(body)
		out[name] = cols
	}
	if len(out) == 0 {
		t.Fatalf("parseSchemaForTest: no CREATE TABLE matched")
	}
	return out
}

func parseColumnsFromBodyForTest(body string) []string {
	var cols []string
	depth := 0
	cur := strings.Builder{}
	flush := func() {
		token := strings.TrimSpace(cur.String())
		cur.Reset()
		if token == "" {
			return
		}
		upper := strings.ToUpper(token)
		// Skip constraint definitions; we only care about column names.
		for _, kw := range []string{"PRIMARY KEY", "UNIQUE", "FOREIGN KEY", "KEY ", "CONSTRAINT ", "CHECK ", "INDEX "} {
			if strings.HasPrefix(upper, kw) {
				return
			}
		}
		// The first whitespace-separated token is the column name.
		fields := strings.Fields(token)
		if len(fields) == 0 {
			return
		}
		name := strings.Trim(fields[0], "`\"")
		cols = append(cols, strings.ToLower(name))
	}
	for _, r := range body {
		switch r {
		case '(':
			depth++
			cur.WriteRune(r)
		case ')':
			depth--
			cur.WriteRune(r)
		case ',':
			if depth == 0 {
				flush()
				continue
			}
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return cols
}

func tableNameDiff(a, b map[string][]string) string {
	keysA := keysOf(a)
	keysB := keysOf(b)
	sort.Strings(keysA)
	sort.Strings(keysB)
	missingInB := setSub(keysA, keysB)
	missingInA := setSub(keysB, keysA)
	if len(missingInA) == 0 && len(missingInB) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(missingInB) > 0 {
		sb.WriteString("  tables present only in SQLite: " + strings.Join(missingInB, ", ") + "\n")
	}
	if len(missingInA) > 0 {
		sb.WriteString("  tables present only in MySQL:  " + strings.Join(missingInA, ", ") + "\n")
	}
	return sb.String()
}

func columnNameDiff(a, b []string) string {
	sa := lowerSet(a)
	sb := lowerSet(b)
	missingInB := setSub(sa, sb)
	missingInA := setSub(sb, sa)
	if len(missingInA) == 0 && len(missingInB) == 0 {
		return ""
	}
	var sb2 strings.Builder
	if len(missingInB) > 0 {
		sb2.WriteString("  columns present only in SQLite: " + strings.Join(missingInB, ", ") + "\n")
	}
	if len(missingInA) > 0 {
		sb2.WriteString("  columns present only in MySQL:  " + strings.Join(missingInA, ", ") + "\n")
	}
	return sb2.String()
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func lowerSet(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	sort.Strings(out)
	return out
}

func setSub(a, b []string) []string {
	bset := make(map[string]struct{}, len(b))
	for _, x := range b {
		bset[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := bset[x]; !ok {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
