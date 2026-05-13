package store

import (
	"database/sql"
)

// dbHandle wraps *sql.DB with dialect-aware SQL rewriting. Every call
// passes the SQL through `dialect.Rewrite` and the args through the
// datetime-offset arg rewriter before reaching the driver, so call sites
// can keep writing SQLite-flavoured SQL and remain backend-agnostic.
//
// `*sql.DB` is embedded so methods we don't override (Ping, Close, Stats,
// Begin, Driver) pass through unchanged. The handful that matter
// (Exec/ExecContext/Query/QueryContext/QueryRow/QueryRowContext) are
// shadowed so the rewrite is unmissable.
//
// Transactions returned by Begin/BeginTx are wrapped in `txHandle` for
// the same reason.
type dbHandle struct {
	*sql.DB
	dialect dialect
}

func newDBHandle(db *sql.DB, d dialect) *dbHandle {
	return &dbHandle{DB: db, dialect: d}
}

func (h *dbHandle) rewrite(query string, args []any) (string, []any) {
	q := h.dialect.Rewrite(query)
	if h.dialect.kind == dialectSQLite || len(args) == 0 {
		return q, args
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = h.dialect.RewriteDatetimeOffsetArg(a)
	}
	return q, out
}

func (h *dbHandle) Exec(query string, args ...any) (sql.Result, error) {
	q, a := h.rewrite(query, args)
	return h.DB.Exec(q, a...)
}

func (h *dbHandle) Query(query string, args ...any) (*sql.Rows, error) {
	q, a := h.rewrite(query, args)
	return h.DB.Query(q, a...)
}

func (h *dbHandle) QueryRow(query string, args ...any) *sql.Row {
	q, a := h.rewrite(query, args)
	return h.DB.QueryRow(q, a...)
}

// Begin returns a dialect-aware transaction wrapper.
func (h *dbHandle) Begin() (*txHandle, error) {
	tx, err := h.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &txHandle{Tx: tx, dialect: h.dialect}, nil
}

// txHandle is the transaction-level counterpart of dbHandle. It applies
// the same SQL/arg rewrites for queries issued inside a transaction.
type txHandle struct {
	*sql.Tx
	dialect dialect
}

func (h *txHandle) rewrite(query string, args []any) (string, []any) {
	q := h.dialect.Rewrite(query)
	if h.dialect.kind == dialectSQLite || len(args) == 0 {
		return q, args
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = h.dialect.RewriteDatetimeOffsetArg(a)
	}
	return q, out
}

func (h *txHandle) Exec(query string, args ...any) (sql.Result, error) {
	q, a := h.rewrite(query, args)
	return h.Tx.Exec(q, a...)
}

func (h *txHandle) Query(query string, args ...any) (*sql.Rows, error) {
	q, a := h.rewrite(query, args)
	return h.Tx.Query(q, a...)
}

func (h *txHandle) QueryRow(query string, args ...any) *sql.Row {
	q, a := h.rewrite(query, args)
	return h.Tx.QueryRow(q, a...)
}

// Prepare rewrites once and prepares the statement on the driver.
// Statements returned by Prepare share the dialect-rewritten SQL; their
// Exec/Query calls then use the driver's prepared form so arg rewriting
// (datetime offsets) still applies via a shim.
func (h *txHandle) Prepare(query string) (*stmtHandle, error) {
	q := h.dialect.Rewrite(query)
	stmt, err := h.Tx.Prepare(q)
	if err != nil {
		return nil, err
	}
	return &stmtHandle{Stmt: stmt, dialect: h.dialect}, nil
}

// stmtHandle wraps *sql.Stmt to rewrite arguments (the SQL is already
// rewritten and prepared at the driver level).
type stmtHandle struct {
	*sql.Stmt
	dialect dialect
}

func (h *stmtHandle) Exec(args ...any) (sql.Result, error) {
	if h.dialect.kind == dialectSQLite {
		return h.Stmt.Exec(args...)
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = h.dialect.RewriteDatetimeOffsetArg(a)
	}
	return h.Stmt.Exec(out...)
}
