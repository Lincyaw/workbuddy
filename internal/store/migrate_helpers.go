// Package store: migrate-time helpers.
//
// This file exposes a tiny export surface used by `workbuddy db migrate`
// (cmd/db_migrate.go). The migrate command runs outside the per-Store
// connection — it streams rows from one Store into another via raw
// `SELECT col, ... FROM table` followed by `INSERT INTO table (cols)
// VALUES (?, ?, ...)`. That pipeline bypasses the typed repository
// methods that already route timestamp writes through
// `dialect.FormatTimestamp` (REQ-154), so SQLite-side RFC3339 strings
// like `2026-04-18T11:30:45Z` flow straight into MySQL `DATETIME(6)`
// columns and are rejected under `STRICT_TRANS_TABLES`:
//
//	ERROR 1292 (22007): Incorrect datetime value: '2026-04-18T11:30:45Z'
//
// The fix-shape lives in cmd/db_migrate.go: classify destination
// timestamp columns up front, then rewrite each scanned timestamp
// value through the destination dialect's formatter before binding it
// into the INSERT. The dialect machinery itself is package-private, so
// this file is the narrow seam that exposes the formatter to cmd/
// without leaking the dialect type or relaxing the Store interface.
//
// Why this helper lives in internal/store/ (not cmd/):
//
//   - The DSN-scheme → dialect mapping is the store package's
//     responsibility (newWithMode in store.go is the canonical mapper).
//     Duplicating it in cmd/ would create two sources of truth for
//     which DSN prefix means MySQL.
//   - The format-string itself is the store package's invariant
//     (dialect.FormatTimestamp in dialect.go). cmd/ should not know
//     that MySQL wants `2006-01-02 15:04:05.000000`.
//   - Other future migration tools (reverse migrate, dump/restore,
//     adhoc copy scripts) will want the same helper.

package store

import (
	"strings"
	"time"
)

// FormatTimestampForDSN returns the dialect-appropriate textual form for
// the given time, selecting the dialect from the DSN scheme prefix using
// the same rules as `New` (sqlite:// → SQLite; mysql:// → MySQL; bare
// path → SQLite). Output is identical to what would be written by a
// `Store` opened against the same DSN going through
// `dialect.FormatTimestamp`.
//
// This is the public seam for `workbuddy db migrate`: the migrator
// classifies its destination once, builds a formatter closure, and
// rewrites timestamp values out of the SQLite source into the MySQL
// destination's native literal grammar.
func FormatTimestampForDSN(dsn string, t time.Time) string {
	return dialectForDSN(dsn).FormatTimestamp(t)
}

// dialectForDSN is the shared DSN-scheme → dialect classifier. Kept
// private; callers go through FormatTimestampForDSN (or future named
// helpers) so the dialect type stays internal.
func dialectForDSN(dsn string) dialect {
	switch {
	case strings.HasPrefix(dsn, "mysql://"):
		return dialect{kind: dialectMySQL}
	default:
		// `sqlite://` and bare paths both map to SQLite, matching
		// newWithMode in store.go.
		return dialect{kind: dialectSQLite}
	}
}
