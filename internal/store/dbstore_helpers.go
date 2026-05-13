package store

import (
	"context"
	"database/sql"
	"strings"
)

// rewriteArgs rewrites argument values for dialect-specific call shapes —
// chiefly the `datetime('now', ?)` modifier whose parameter form
// ("+N seconds" / "-N seconds") differs between SQLite and MySQL. The
// rewrite is content-driven, not position-driven: only strings shaped like
// the SQLite modifier are touched, so passing a normal timestamp or text
// parameter is safe.
//
// Identity on SQLite.
func (s *dbStore) rewriteArgs(args []any) []any {
	if s == nil || s.dialect.kind == dialectSQLite || len(args) == 0 {
		return args
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = s.dialect.RewriteDatetimeOffsetArg(a)
	}
	return out
}

// rewriteSQL is a one-stop helper: dialect rewrite plus, for MySQL, an
// internal arg-side adjustment hook. The store's Exec/Query/QueryRow
// methods funnel through this so production code never has to reach for
// dialect details.
func (s *dbStore) rewriteSQL(sql string) string {
	if s == nil {
		return sql
	}
	return s.dialect.Rewrite(sql)
}

// execRewritten is a convenience used internally where the SQL/arg
// transformations are not yet wired through the public Exec method. It
// applies the dialect rewrite, then delegates to the underlying *sql.DB.
func (s *dbStore) execRewritten(ctx context.Context, query string, args ...any) (sql.Result, error) {
	q := s.rewriteSQL(query)
	rArgs := s.rewriteArgs(args)
	if ctx == nil {
		return s.db.Exec(q, rArgs...)
	}
	return s.db.ExecContext(ctx, q, rArgs...)
}

// isMySQL is a short-hand used by the small number of methods that take a
// different code path on MySQL (the `RETURNING` emulation in
// IncrementTransition / IncrementDevReviewCycleCount /
// IncrementSynthCycleCount, and the `migrateLegacySessions` shape).
func (s *dbStore) isMySQL() bool {
	return s != nil && s.dialect.kind == dialectMySQL
}

// trimSchemaSQL returns non-empty statements from a SQL script split on
// ";". MySQL clients don't support multiple statements in a single Exec
// unless `multiStatements=true`; splitting on the client side avoids
// requiring that DSN flag for the bootstrap path.
func trimSchemaSQL(script string) []string {
	parts := strings.Split(script, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		stmt := strings.TrimSpace(p)
		if stmt == "" {
			continue
		}
		// Allow line comments in the schema file.
		lines := strings.Split(stmt, "\n")
		kept := make([]string, 0, len(lines))
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "--") {
				continue
			}
			kept = append(kept, line)
		}
		stmt = strings.TrimSpace(strings.Join(kept, "\n"))
		if stmt == "" {
			continue
		}
		out = append(out, stmt)
	}
	return out
}
