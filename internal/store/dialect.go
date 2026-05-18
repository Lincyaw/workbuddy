// Package store: SQL dialect abstraction.
//
// The store package is the abstract persistence boundary for workbuddy. To
// keep call sites untouched (the Store interface is the contract) while
// supporting both SQLite (systemd default) and MySQL (K8s default, per
// docs/decisions/2026-05-13-k8s-agentm-otel.md Block 3 § Storage), the
// concrete implementation `dbStore` is parameterised by a `dialect` value
// that knows how to:
//
//   - Open the driver (driver name + DSN normalisation).
//   - Translate a small set of dialect-specific SQL fragments
//     (`INSERT OR IGNORE`, `ON CONFLICT … DO UPDATE SET … excluded.col`,
//     `datetime('now', ?)`, `julianday(...)`, `RETURNING …`) on the fly.
//   - Recognise transient busy errors and duplicate-column ALTER errors,
//     which surface with different messages on different drivers.
//   - Return the per-engine schema bootstrap DDL (separate `schema.sql` for
//     SQLite vs. MySQL; see `internal/store/mysql/schema.sql`).
//
// All SQL written in the rest of the package is the SQLite source-of-truth.
// MySQL deviations are produced by `dialect.Rewrite(sql)` at exec time. This
// keeps the diff for #316 small and gives us a single textual schema to
// compare against (`TestSchemaParity`).
package store

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// dialectKind names the concrete backend. It controls SQL rewriting,
// driver selection, and which schema file is applied on bootstrap.
type dialectKind int

const (
	dialectSQLite dialectKind = iota
	dialectMySQL
)

func (d dialectKind) String() string {
	switch d {
	case dialectMySQL:
		return "mysql"
	default:
		return "sqlite"
	}
}

// dialect captures the small surface of per-engine behaviour the store needs.
type dialect struct {
	kind dialectKind
}

// Rewrite translates SQLite-flavoured SQL to the target dialect. For SQLite
// it is the identity. For MySQL it applies a fixed set of substitutions for
// the constructs used in this package; see the inline comments for each
// branch and the schema parity test for coverage.
func (d dialect) Rewrite(sql string) string {
	if d.kind == dialectSQLite {
		return sql
	}
	return rewriteForMySQL(sql)
}

// IsDuplicateColumnError reports whether err came from an ADD COLUMN that
// the table already had. SQLite says "duplicate column name"; MySQL says
// "Duplicate column name" / error 1060.
func (d dialect) IsDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Both drivers produce a substring containing "duplicate column".
	if strings.Contains(msg, "duplicate column") {
		return true
	}
	// MySQL also surfaces error code 1060.
	if strings.Contains(msg, "error 1060") {
		return true
	}
	return false
}

// IsBusyError reports whether err is a transient contention error that the
// store retries (SQLITE_BUSY on SQLite; MySQL deadlock / lock wait timeout).
func (d dialect) IsBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if d.kind == dialectSQLite {
		return strings.Contains(msg, "database is locked") ||
			strings.Contains(msg, "database table is locked") ||
			strings.Contains(msg, "sqlite_busy")
	}
	// MySQL
	return strings.Contains(msg, "deadlock found") ||
		strings.Contains(msg, "lock wait timeout") ||
		strings.Contains(msg, "try restarting transaction")
}

// IsUniqueConstraintError reports whether err is a UNIQUE / PK collision.
func (d dialect) IsUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "primary key") ||
		strings.Contains(msg, "constraint failed") {
		return true
	}
	// MySQL: ER_DUP_ENTRY 1062.
	return strings.Contains(msg, "duplicate entry") || strings.Contains(msg, "error 1062")
}

// ---------------------------------------------------------------------------
// MySQL rewrite rules
// ---------------------------------------------------------------------------
//
// The rules below cover every dialect-specific fragment used in this
// package today. The schema parity test exercises a representative subset;
// new fragments should be added here together with a parity-test case.

var (
	// `INSERT OR IGNORE INTO` → `INSERT IGNORE INTO`.
	reInsertOrIgnore = regexp.MustCompile(`(?i)\bINSERT\s+OR\s+IGNORE\s+INTO\b`)
	// `INSERT OR REPLACE INTO` → `REPLACE INTO`.
	reInsertOrReplace = regexp.MustCompile(`(?i)\bINSERT\s+OR\s+REPLACE\s+INTO\b`)
	// `datetime('now', ?)` → `(NOW(6) + INTERVAL ? SECOND_MICROSECOND)`.
	// All call sites in this package format the modifier as
	// "+<N> seconds" / "-<N> seconds". We translate those parameter values
	// at exec time via the argument rewriter rather than re-shaping the
	// SQL string, because the modifier is bound as a parameter not a
	// literal. See dialect.RewriteArgs below.
	reDatetimeNow = regexp.MustCompile(`(?i)datetime\(\s*'now'\s*,\s*\?\s*\)`)
	// `julianday(X)` → `UNIX_TIMESTAMP(X) / 86400.0` would round, so use
	// `TIMESTAMPDIFF(SECOND, a, b)` on the surrounding expression instead.
	// The single call site computes `(julianday(b) - julianday(a)) * 86400.0`
	// to yield seconds-with-fraction. We rewrite that exact form to
	// `TIMESTAMPDIFF(MICROSECOND, a, b) / 1000000.0` which keeps the units.
	reJulianDeltaSeconds = regexp.MustCompile(
		`\(\s*julianday\(\s*([^)]+?)\s*\)\s*-\s*julianday\(\s*([^)]+?)\s*\)\s*\)\s*\*\s*86400(?:\.0)?`,
	)
	// `RETURNING <col>` is supported by SQLite ≥ 3.35 and PostgreSQL but
	// not by MySQL. The two call sites that use it (IncrementDevReviewCycleCount
	// and IncrementSynthCycleCount and the transition counts RETURNING)
	// are emulated with a SELECT after the upsert; rather than try to
	// rewrite arbitrary `RETURNING` shapes here, the call sites detect
	// MySQL via `s.dialect.kind` and take an alternate code path.
	//
	// `ON CONFLICT (cols) DO UPDATE SET col = excluded.col, ...` is the
	// only generic upsert form we use. MySQL spells the same thing as
	// `ON DUPLICATE KEY UPDATE col = VALUES(col), ...`. The rewrite drops
	// the `(cols)` target (MySQL uses every unique key as the conflict
	// target) and converts `excluded.X` → `VALUES(X)`.
	reOnConflictDoUpdate = regexp.MustCompile(
		`(?is)ON\s+CONFLICT\s*\([^)]*\)\s*DO\s+UPDATE\s+SET\b`,
	)
	reOnConflictDoNothing = regexp.MustCompile(
		`(?is)ON\s+CONFLICT\s*(?:\([^)]*\))?\s*DO\s+NOTHING`,
	)
	reExcluded = regexp.MustCompile(`(?i)\bexcluded\.([a-zA-Z_][a-zA-Z0-9_]*)`)
)

func rewriteForMySQL(sql string) string {
	out := sql

	// `ON CONFLICT ... DO NOTHING` becomes `INSERT IGNORE`. We achieve
	// this by stripping the trailing clause and ensuring the statement
	// uses `INSERT IGNORE INTO` at the head. Order matters: this must
	// run before the `INSERT INTO`-fragment substitutions don't accidentally
	// reapply.
	if reOnConflictDoNothing.MatchString(out) {
		out = reOnConflictDoNothing.ReplaceAllString(out, "")
		out = strings.TrimSpace(out)
		// Upgrade the leading INSERT (only if it isn't already INSERT IGNORE
		// or INSERT OR IGNORE) to INSERT IGNORE.
		upper := strings.ToUpper(out)
		switch {
		case strings.HasPrefix(upper, "INSERT IGNORE"):
			// already there
		case strings.HasPrefix(upper, "INSERT OR IGNORE"):
			// handled by the OR-IGNORE rewrite below
		case strings.HasPrefix(upper, "INSERT INTO"):
			out = "INSERT IGNORE INTO" + out[len("INSERT INTO"):]
		}
	}

	out = reInsertOrIgnore.ReplaceAllString(out, "INSERT IGNORE INTO")
	out = reInsertOrReplace.ReplaceAllString(out, "REPLACE INTO")

	// ON CONFLICT (cols) DO UPDATE SET → ON DUPLICATE KEY UPDATE
	out = reOnConflictDoUpdate.ReplaceAllString(out, "ON DUPLICATE KEY UPDATE")
	// excluded.col → VALUES(col)
	out = reExcluded.ReplaceAllString(out, "VALUES($1)")

	// julianday delta → MySQL TIMESTAMPDIFF in microseconds.
	out = reJulianDeltaSeconds.ReplaceAllStringFunc(out, func(m string) string {
		sub := reJulianDeltaSeconds.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		// julianday(b) - julianday(a) means "later minus earlier", so
		// TIMESTAMPDIFF expects (earlier, later) = (sub[2], sub[1]).
		return fmt.Sprintf("(TIMESTAMPDIFF(MICROSECOND, %s, %s) / 1000000.0)", strings.TrimSpace(sub[2]), strings.TrimSpace(sub[1]))
	})

	// datetime('now', ?) → DATE_ADD(NOW(6), INTERVAL <param> SECOND).
	// The parameter still comes in as a "+/-N seconds" string; the runtime
	// argument rewriter (dialect.RewriteDatetimeOffsetArg) converts it to
	// a plain integer. The placeholder count is preserved so positional
	// binding stays in sync.
	out = reDatetimeNow.ReplaceAllString(out, "DATE_ADD(NOW(6), INTERVAL ? SECOND)")

	return out
}

// DatetimeNormalize wraps a DATETIME-bearing SQL expression in a form that
// compares layout-independently against another datetime value. It exists to
// neutralise a specific SQLite hazard: TEXT-typed DATETIME columns store the
// raw bytes the writer hands them (modernc.org/sqlite does no on-insert
// normalisation), and `WHERE col > ?` is a lexicographic byte compare, so a
// column whose rows are a mix of space-form ("2026-04-18 11:30:00") and
// RFC3339 ("2026-04-18T11:30:00Z") will mis-sort against a same-day cutoff
// because 'T' (0x54) > ' ' (0x20). Wrapping both sides in SQLite's
// `datetime(...)` parses either layout and normalises to a single comparable
// form before the compare, so an upgrade-in-place where pre-W2 (space) rows
// coexist with post-W2 (RFC3339) rows still classifies correctly without a
// data migration.
//
// On MySQL the columns are typed DATETIME(6); the driver+server parse the
// bound string into a DATETIME, the compare is typed, and there is no
// layout-text-compare hazard, so the wrapper is the identity. Keep MySQL on
// the identity path to avoid pinning the SQL on a function that has different
// semantics across engines.
func (d dialect) DatetimeNormalize(expr string) string {
	if d.kind == dialectSQLite {
		return "datetime(" + expr + ")"
	}
	return expr
}

// RewriteDatetimeOffsetArg converts a SQLite-style "+N seconds" / "-N seconds"
// modifier (used with datetime('now', ?)) into the bare integer N that
// MySQL's `INTERVAL ? SECOND` expects. Identity on SQLite. Returns the input
// unchanged for values it does not recognise (e.g. tests that pass a
// pre-formatted timestamp directly).
func (d dialect) RewriteDatetimeOffsetArg(arg any) any {
	if d.kind == dialectSQLite {
		return arg
	}
	s, ok := arg.(string)
	if !ok {
		return arg
	}
	s = strings.TrimSpace(s)
	if !strings.HasSuffix(s, " seconds") && !strings.HasSuffix(s, " second") {
		return arg
	}
	num := strings.TrimSuffix(s, " seconds")
	num = strings.TrimSuffix(num, " second")
	num = strings.TrimSpace(num)
	// num may start with + or -.
	n, err := strconv.Atoi(num)
	if err != nil {
		return arg
	}
	return n
}

// ErrUnsupportedScheme is returned by New() when the DSN scheme is not
// recognised.
var ErrUnsupportedScheme = errors.New("store: unsupported DSN scheme")
