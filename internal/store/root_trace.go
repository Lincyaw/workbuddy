// Package store: root_trace_id minting and accessors (REQ-137 / #317).
//
// Long-lifecycle OTel correlation works by reusing a single trace_id
// across every operation on an issue or PR. The trace_id is generated
// on first ingest of the row into issue_cache and stored alongside
// labels/state. Subsequent operations look it up via
// GetIssueRootTraceID / GetPRRootTraceID and use it to parent child
// spans.
//
// This file owns:
//   - newRootTraceID(): generate a 32-hex trace_id, preferring the
//     active OTel tracer (so the value appears in the collector) and
//     falling back to crypto/rand when no SDK is installed.
//   - GetIssueRootTraceID / GetPRRootTraceID: read accessors on
//     dbStore (the issue_cache row holds the value for both —
//     issues and PRs are differentiated only by the "pr:" prefix on
//     state, so the same SELECT serves both with different keys).
//
// Span construction and cross-span correlation logic is intentionally
// NOT here — that lands in #320.

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/Lincyaw/workbuddy/internal/tracing"
)

// newRootTraceID returns a freshly minted 32-character lowercase hex
// trace_id. It opens and immediately closes a brief OTel span via the
// global tracer; if that yields a valid trace_id it is used so the
// trace_id is reachable in the configured collector. If no SDK is
// installed (e.g. unit tests without tracing.Init) the tracer returns
// the zero trace_id and we fall back to crypto/rand so callers always
// get a usable 32-hex value.
func newRootTraceID() string {
	_, span := tracing.Start(context.Background(), "store.mint_root_trace_id")
	tid := span.SpanContext().TraceID()
	span.End()
	if tid.IsValid() {
		return tid.String()
	}
	// Fallback: 16 random bytes -> 32-hex. Matches OTel trace_id shape.
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is extraordinary; surface as a zero-but-
		// non-empty marker so the row still passes the non-empty
		// invariant. Logging here is intentionally avoided to keep this
		// helper hot-path-safe.
		return "00000000000000000000000000000001"
	}
	return hex.EncodeToString(b[:])
}

// GetIssueRootTraceID returns the persisted root_trace_id for an issue
// row in issue_cache. Returns "" and nil error when the row does not
// exist yet (caller decides whether absence is fatal).
func (s *dbStore) GetIssueRootTraceID(repo string, issueNum int) (string, error) {
	return s.getRootTraceID(repo, issueNum)
}

// GetPRRootTraceID returns the persisted root_trace_id for a PR row.
// PRs share issue_cache with issues — the "pr:" prefix on state is the
// only discriminator — so this is the same lookup keyed by PR number.
func (s *dbStore) GetPRRootTraceID(repo string, prNum int) (string, error) {
	return s.getRootTraceID(repo, prNum)
}

func (s *dbStore) getRootTraceID(repo string, num int) (string, error) {
	var tid string
	err := s.db.QueryRow(
		`SELECT root_trace_id FROM issue_cache WHERE repo = ? AND issue_num = ?`,
		repo, num,
	).Scan(&tid)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get root_trace_id for %s#%d: %w", repo, num, err)
	}
	return tid, nil
}
