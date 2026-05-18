package store

import (
	"strings"
	"testing"
	"time"
)

// TestFormatTimestamp_SQLite asserts the SQLite dialect emits RFC3339, which
// is the layout produced by nullableTime and the layout the rest of the store
// package compares against in WHERE cutoffs (lexicographic TEXT compare on
// SQLite). The output must end with 'Z' (UTC zone designator), use 'T' as the
// date/time separator, and lossy-round to seconds — exactly what RFC3339
// specifies.
func TestFormatTimestamp_SQLite(t *testing.T) {
	d := dialect{kind: dialectSQLite}
	in := time.Date(2026, 4, 18, 11, 30, 45, 123456789, time.UTC)
	got := d.FormatTimestamp(in)
	want := "2026-04-18T11:30:45Z"
	if got != want {
		t.Fatalf("FormatTimestamp(SQLite) = %q, want %q", got, want)
	}
}

// TestFormatTimestamp_SQLite_NonUTC verifies the helper forces UTC. A local
// timezone input must be converted before formatting; otherwise downstream
// WHERE cutoffs would mis-sort against UTC-stamped rows.
func TestFormatTimestamp_SQLite_NonUTC(t *testing.T) {
	d := dialect{kind: dialectSQLite}
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("LoadLocation: %v", err)
	}
	// 2026-04-18 04:30:00 PDT = 2026-04-18 11:30:00 UTC.
	in := time.Date(2026, 4, 18, 4, 30, 0, 0, loc)
	got := d.FormatTimestamp(in)
	want := "2026-04-18T11:30:00Z"
	if got != want {
		t.Fatalf("FormatTimestamp(SQLite, non-UTC) = %q, want %q", got, want)
	}
}

// TestFormatTimestamp_MySQL asserts the MySQL dialect emits the
// 'YYYY-MM-DD HH:MM:SS.ffffff' literal grammar that MySQL's DATETIME(6)
// parser accepts under strict sql_mode. Specifically:
//   - The date/time separator is a SPACE, not 'T'.
//   - There is NO trailing zone designator ('Z' or '+00:00').
//   - The fractional second is present and 6 digits wide (DATETIME(6)).
// Under STRICT_TRANS_TABLES (MySQL 8's default sql_mode), any of the
// forbidden constructs causes "Incorrect datetime value: '...'" and the
// INSERT is rejected — the exact silent-failure mode this helper exists to
// prevent.
func TestFormatTimestamp_MySQL(t *testing.T) {
	d := dialect{kind: dialectMySQL}
	in := time.Date(2026, 4, 18, 11, 30, 45, 123456789, time.UTC)
	got := d.FormatTimestamp(in)
	want := "2026-04-18 11:30:45.123456"
	if got != want {
		t.Fatalf("FormatTimestamp(MySQL) = %q, want %q", got, want)
	}
	// Defensive: explicitly assert the forbidden characters are absent so a
	// future refactor that switches to time.RFC3339Nano (which keeps 'T' and
	// 'Z') trips this test rather than silently shipping the bug.
	if strings.ContainsAny(got, "TZ") {
		t.Fatalf("FormatTimestamp(MySQL) %q must not contain 'T' or 'Z'", got)
	}
}

// TestFormatTimestamp_MySQL_NonUTC verifies the MySQL branch also forces UTC.
func TestFormatTimestamp_MySQL_NonUTC(t *testing.T) {
	d := dialect{kind: dialectMySQL}
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("LoadLocation: %v", err)
	}
	in := time.Date(2026, 4, 18, 4, 30, 0, 0, loc)
	got := d.FormatTimestamp(in)
	want := "2026-04-18 11:30:00.000000"
	if got != want {
		t.Fatalf("FormatTimestamp(MySQL, non-UTC) = %q, want %q", got, want)
	}
}

// TestFormatTimestamp_RoundTrip pins the read-path contract: whatever a
// dialect writes via FormatTimestamp must be re-parseable by the shared
// ParseTimestamp helper. This protects future dialect additions (or layout
// tweaks) from silently breaking every Scan-side timestamp consumer.
func TestFormatTimestamp_RoundTrip(t *testing.T) {
	in := time.Date(2026, 4, 18, 11, 30, 45, 0, time.UTC)
	for _, tc := range []struct {
		name string
		d    dialect
	}{
		{"sqlite", dialect{kind: dialectSQLite}},
		{"mysql", dialect{kind: dialectMySQL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.d.FormatTimestamp(in)
			got, ok := ParseTimestamp(raw, "test."+tc.name)
			if !ok {
				t.Fatalf("ParseTimestamp(%q) failed", raw)
			}
			// MySQL layout has sub-second precision, SQLite RFC3339 does not;
			// truncate to seconds before comparison so both branches use the
			// same comparison granularity.
			if !got.Truncate(time.Second).Equal(in.Truncate(time.Second)) {
				t.Fatalf("round-trip mismatch: in=%v raw=%q parsed=%v", in, raw, got)
			}
		})
	}
}

// TestIssueClaimRoundTripPersists exercises the production write path through
// a real SQLite store: AcquireIssueClaim writes both acquired_at and
// expires_at via the new dialect helper, RefreshIssueClaim extends
// expires_at, and QueryIssueClaim must scan the values back. The original
// bug class would manifest as either a write failure (MySQL) or a Scan-side
// parse failure (any dialect that produced an un-parseable layout); this
// test pins the SQLite contract and the MySQL integration test below
// exercises the MySQL contract.
func TestIssueClaimRoundTripPersists(t *testing.T) {
	s := newTestStore(t)
	res, err := s.AcquireIssueClaim("owner/repo", 42, "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireIssueClaim: %v", err)
	}
	if res.ClaimToken == "" {
		t.Fatalf("expected non-empty claim token")
	}
	rec, err := s.QueryIssueClaim("owner/repo", 42)
	if err != nil {
		t.Fatalf("QueryIssueClaim: %v", err)
	}
	if rec == nil {
		t.Fatalf("expected claim row, got nil")
	}
	if rec.AcquiredAt.IsZero() || rec.ExpiresAt.IsZero() {
		t.Fatalf("expected non-zero timestamps, got acquired_at=%v expires_at=%v",
			rec.AcquiredAt, rec.ExpiresAt)
	}
	if !rec.ExpiresAt.After(rec.AcquiredAt) {
		t.Fatalf("expires_at %v must be after acquired_at %v",
			rec.ExpiresAt, rec.AcquiredAt)
	}
	priorExpiry := rec.ExpiresAt

	// Refresh extends expires_at — read-side parse must still succeed.
	time.Sleep(10 * time.Millisecond)
	ok, err := s.RefreshIssueClaim("owner/repo", 42, "worker-1", res.ClaimToken, 5*time.Minute)
	if err != nil {
		t.Fatalf("RefreshIssueClaim: %v", err)
	}
	if !ok {
		t.Fatalf("RefreshIssueClaim returned false; expected the row to update")
	}
	after, err := s.QueryIssueClaim("owner/repo", 42)
	if err != nil {
		t.Fatalf("QueryIssueClaim after refresh: %v", err)
	}
	if after == nil {
		t.Fatalf("expected claim row after refresh, got nil")
	}
	if !after.ExpiresAt.After(priorExpiry) {
		t.Fatalf("expected expires_at to advance, prior=%v after=%v",
			priorExpiry, after.ExpiresAt)
	}
}
