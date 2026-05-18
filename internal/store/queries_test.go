package store

import (
	"testing"
	"time"
)

// TestQueryEventsFiltered exercises the new narrow event query used by
// eventlog.EventLogger.Query.
func TestQueryEventsFiltered(t *testing.T) {
	s := newTestStore(t)

	mustInsertEvent(t, s, Event{Type: "poll", Repo: "org/a", IssueNum: 1, Payload: `{}`})
	mustInsertEvent(t, s, Event{Type: "dispatch", Repo: "org/a", IssueNum: 1, Payload: `{}`})
	mustInsertEvent(t, s, Event{Type: "poll", Repo: "org/b", IssueNum: 2, Payload: `{}`})

	all, err := s.QueryEventsFiltered(EventQueryFilter{})
	if err != nil {
		t.Fatalf("QueryEventsFiltered all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 events, got %d", len(all))
	}

	repoA, err := s.QueryEventsFiltered(EventQueryFilter{Repo: "org/a"})
	if err != nil {
		t.Fatalf("QueryEventsFiltered repoA: %v", err)
	}
	if len(repoA) != 2 {
		t.Fatalf("expected 2 events for org/a, got %d", len(repoA))
	}

	poll, err := s.QueryEventsFiltered(EventQueryFilter{Type: "poll"})
	if err != nil {
		t.Fatalf("QueryEventsFiltered type: %v", err)
	}
	if len(poll) != 2 {
		t.Fatalf("expected 2 poll events, got %d", len(poll))
	}

	issue1, err := s.QueryEventsFiltered(EventQueryFilter{IssueNum: 1})
	if err != nil {
		t.Fatalf("QueryEventsFiltered issue: %v", err)
	}
	if len(issue1) != 2 {
		t.Fatalf("expected 2 events for issue 1, got %d", len(issue1))
	}
}

// TestQueryEventsFilteredTimestamp pins the regression from issue #345:
// QueryEventsFiltered used a rigid parse layout, so ev.TS silently became
// the zero value when the driver returned RFC3339. After the fix it must
// round-trip through ParseTimestamp.
func TestQueryEventsFilteredTimestamp(t *testing.T) {
	s := newTestStore(t)

	before := time.Now().UTC().Add(-1 * time.Minute)
	mustInsertEvent(t, s, Event{Type: "poll", Repo: "org/a", IssueNum: 1, Payload: `{}`})

	events, err := s.QueryEventsFiltered(EventQueryFilter{})
	if err != nil {
		t.Fatalf("QueryEventsFiltered: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	got := events[0].TS
	if got.IsZero() {
		t.Fatalf("expected non-zero TS, got zero value (parse layout mismatch?)")
	}
	if got.Before(before) {
		t.Fatalf("expected TS >= %v (last minute), got %v", before, got)
	}
}

// TestQueryEventsFilteredSinceUntil pins the second half of #345: events.ts
// is written by SQLite's CURRENT_TIMESTAMP default as the space-separated
// form 'YYYY-MM-DD HH:MM:SS' (not RFC3339), and SQLite compares TEXT
// lexicographically. The Since/Until WHERE args must use the same layout
// so the boundary doesn't mis-order against 'T'.
//
// To get deterministic ts values we overwrite events.ts via the Store's
// Exec passthrough after insertion — CURRENT_TIMESTAMP would set it to
// "now" and the boundaries we want to test sit decades ago.
func TestQueryEventsFilteredSinceUntil(t *testing.T) {
	s := newTestStore(t)

	id1, err := s.InsertEvent(Event{Type: "poll", Repo: "org/a", IssueNum: 1, Payload: `{}`})
	if err != nil {
		t.Fatalf("InsertEvent 1: %v", err)
	}
	id2, err := s.InsertEvent(Event{Type: "poll", Repo: "org/a", IssueNum: 1, Payload: `{}`})
	if err != nil {
		t.Fatalf("InsertEvent 2: %v", err)
	}
	id3, err := s.InsertEvent(Event{Type: "poll", Repo: "org/a", IssueNum: 1, Payload: `{}`})
	if err != nil {
		t.Fatalf("InsertEvent 3: %v", err)
	}

	for _, row := range []struct {
		id int64
		ts string
	}{
		{id1, "2020-01-01 01:00:00"},
		{id2, "2020-01-01 12:00:00"},
		{id3, "2020-01-01 23:00:00"},
	} {
		if _, err := s.Exec(`UPDATE events SET ts = ? WHERE id = ?`, row.ts, row.id); err != nil {
			t.Fatalf("backdate ts for id=%d: %v", row.id, err)
		}
	}

	// Since boundary: 10:00 — only the 12:00 and 23:00 rows should match.
	since := time.Date(2020, 1, 1, 10, 0, 0, 0, time.UTC)
	got, err := s.QueryEventsFiltered(EventQueryFilter{Since: &since})
	if err != nil {
		t.Fatalf("QueryEventsFiltered Since: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Since=10:00 expected 2 events (12:00 + 23:00), got %d: %+v", len(got), got)
	}
	if got[0].ID != id2 || got[1].ID != id3 {
		t.Fatalf("Since=10:00 expected ids [%d %d], got [%d %d]", id2, id3, got[0].ID, got[1].ID)
	}

	// Until boundary: 10:00 — only the 01:00 row should match.
	until := time.Date(2020, 1, 1, 10, 0, 0, 0, time.UTC)
	got, err = s.QueryEventsFiltered(EventQueryFilter{Until: &until})
	if err != nil {
		t.Fatalf("QueryEventsFiltered Until: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Until=10:00 expected 1 event (01:00), got %d: %+v", len(got), got)
	}
	if got[0].ID != id1 {
		t.Fatalf("Until=10:00 expected id %d, got %d", id1, got[0].ID)
	}
}

// TestCountTerminalSessionsSinceLexicographic pins the third leg of #345:
// sessions.closed_at is written by nullableTime as RFC3339, but the cutoff
// arg used to be formatted "2006-01-02 15:04:05". SQLite compares TEXT
// lexicographically and ' ' (0x20) < 'T' (0x54), so a cutoff like
// "2020-01-01 10:00:00" sorts BEFORE every RFC3339 row on the same date,
// over-counting the same-date / earlier-time-of-day case.
//
// Sessions are inserted via raw INSERTs through Store.Exec so closed_at can
// be pinned to known values that span the boundary.
func TestCountTerminalSessionsSinceLexicographic(t *testing.T) {
	s := newTestStore(t)

	// Three completed sessions on the same date, time-of-day 01:00 / 12:00 / 23:00.
	for i, ts := range []string{
		"2020-01-01T01:00:00Z",
		"2020-01-01T12:00:00Z",
		"2020-01-01T23:00:00Z",
	} {
		if _, err := s.Exec(
			`INSERT INTO sessions (session_id, task_id, repo, issue_num, agent_name, status, closed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"sess-"+ts, "", "org/a", i+1, "dev", TaskStatusCompleted, ts,
		); err != nil {
			t.Fatalf("insert session %s: %v", ts, err)
		}
	}

	// Cutoff: 10:00 on the same date — only the 12:00 and 23:00 rows
	// should count. The old "2006-01-02 15:04:05" cutoff string would
	// sort before every RFC3339 row and return 3 (over-count).
	cutoff := time.Date(2020, 1, 1, 10, 0, 0, 0, time.UTC)
	n, err := s.CountTerminalSessionsSince(TaskStatusCompleted, cutoff)
	if err != nil {
		t.Fatalf("CountTerminalSessionsSince: %v", err)
	}
	if n != 2 {
		t.Fatalf("cutoff=10:00 expected 2 (12:00 + 23:00), got %d", n)
	}

	// Cutoff: 1970-01-01 — all three rows count. Sanity check that the
	// RFC3339 layout doesn't somehow exclude valid rows.
	allCutoff := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	n, err = s.CountTerminalSessionsSince(TaskStatusCompleted, allCutoff)
	if err != nil {
		t.Fatalf("CountTerminalSessionsSince all: %v", err)
	}
	if n != 3 {
		t.Fatalf("cutoff=1970 expected 3, got %d", n)
	}
}

// TestLatestEventAt exercises the helper used by audit.HTTPHandler.
func TestLatestEventAt(t *testing.T) {
	s := newTestStore(t)

	ts, err := s.LatestEventAt("org/a", 1)
	if err != nil {
		t.Fatalf("LatestEventAt missing: %v", err)
	}
	if ts != nil {
		t.Fatalf("expected nil timestamp, got %v", ts)
	}

	mustInsertEvent(t, s, Event{Type: "poll", Repo: "org/a", IssueNum: 1, Payload: `{}`})
	ts, err = s.LatestEventAt("org/a", 1)
	if err != nil {
		t.Fatalf("LatestEventAt present: %v", err)
	}
	if ts == nil {
		t.Fatal("expected non-nil timestamp")
	}
}

// TestMetricsAggregates exercises the aggregate query methods consumed by
// internal/metrics.
func TestMetricsAggregates(t *testing.T) {
	s := newTestStore(t)
	mustInsertEvent(t, s, Event{Type: "poll", Repo: "org/a", IssueNum: 1, Payload: `{}`})
	mustInsertEvent(t, s, Event{Type: "poll", Repo: "org/a", IssueNum: 2, Payload: `{}`})
	mustInsertEvent(t, s, Event{Type: "dispatch", Repo: "org/b", IssueNum: 3, Payload: `{}`})
	mustInsertEvent(t, s, Event{Type: "token_usage", Repo: "org/a", IssueNum: 1, Payload: `{"input":10,"output":5,"total":15}`})

	events, err := s.CountEventsByRepoType()
	if err != nil {
		t.Fatalf("CountEventsByRepoType: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 (repo,type) groups, got %d", len(events))
	}

	tokens, err := s.TokenUsageEvents("token_usage")
	if err != nil {
		t.Fatalf("TokenUsageEvents: %v", err)
	}
	if len(tokens) != 1 || tokens[0].Repo != "org/a" {
		t.Fatalf("unexpected token events: %+v", tokens)
	}

	tasks, err := s.CountTasksByRepoStatus()
	if err != nil {
		t.Fatalf("CountTasksByRepoStatus: %v", err)
	}
	// No tasks inserted: empty aggregate is fine.
	_ = tasks

	workers, err := s.CountWorkersByRepo()
	if err != nil {
		t.Fatalf("CountWorkersByRepo: %v", err)
	}
	_ = workers

	maxCounts, err := s.MaxTransitionCounts()
	if err != nil {
		t.Fatalf("MaxTransitionCounts: %v", err)
	}
	_ = maxCounts

	open, err := s.ListOpenIssueActivity(TaskStatusPending, TaskStatusRunning)
	if err != nil {
		t.Fatalf("ListOpenIssueActivity: %v", err)
	}
	_ = open
}

// TestAuditAPIQueries exercises the auditapi-oriented query methods.
func TestAuditAPIQueries(t *testing.T) {
	s := newTestStore(t)

	active, err := s.CountActiveSessions()
	if err != nil {
		t.Fatalf("CountActiveSessions: %v", err)
	}
	if active != 0 {
		t.Fatalf("expected 0 active sessions, got %d", active)
	}

	workers, err := s.CountWorkers()
	if err != nil {
		t.Fatalf("CountWorkers: %v", err)
	}
	if workers != 0 {
		t.Fatalf("expected 0 workers, got %d", workers)
	}

	ts, err := s.LastEventTimestampByType("poll")
	if err != nil {
		t.Fatalf("LastEventTimestampByType missing: %v", err)
	}
	if ts != nil {
		t.Fatalf("expected nil ts, got %v", ts)
	}
	mustInsertEvent(t, s, Event{Type: "poll", Repo: "org/a", IssueNum: 1, Payload: `{}`})
	ts, err = s.LastEventTimestampByType("poll")
	if err != nil {
		t.Fatalf("LastEventTimestampByType present: %v", err)
	}
	if ts == nil {
		t.Fatal("expected non-nil ts")
	}

	sessions, err := s.ListSessionsForAPI(SessionListFilter{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("ListSessionsForAPI: %v", err)
	}
	_ = sessions

	agg, err := s.AggregateSessionMetrics()
	if err != nil {
		t.Fatalf("AggregateSessionMetrics: %v", err)
	}
	if agg.Total != 0 {
		t.Fatalf("expected 0 sessions, got %d", agg.Total)
	}

	perAgent, err := s.CountSessionsByAgent()
	if err != nil {
		t.Fatalf("CountSessionsByAgent: %v", err)
	}
	if len(perAgent) != 0 {
		t.Fatalf("expected 0 agents, got %d", len(perAgent))
	}
}

// TestWorkflowRepositoryMethods exercises the workflow-oriented methods used
// by workflow.Manager.
func TestWorkflowRepositoryMethods(t *testing.T) {
	s := newTestStore(t)
	id := "org/a#7#default"
	if err := s.CreateWorkflowInstanceIfMissing(id, "default", "org/a", 7, "triage"); err != nil {
		t.Fatalf("CreateWorkflowInstanceIfMissing: %v", err)
	}
	// Idempotent: second call should not error.
	if err := s.CreateWorkflowInstanceIfMissing(id, "default", "org/a", 7, "triage"); err != nil {
		t.Fatalf("CreateWorkflowInstanceIfMissing (repeat): %v", err)
	}

	if err := s.AdvanceWorkflowInstance(id, "triage", "developing", "dev", time.Now()); err != nil {
		t.Fatalf("AdvanceWorkflowInstance: %v", err)
	}

	row, err := s.GetWorkflowInstanceByID(id)
	if err != nil {
		t.Fatalf("GetWorkflowInstanceByID: %v", err)
	}
	if row.CurrentState != "developing" {
		t.Fatalf("expected developing, got %q", row.CurrentState)
	}

	instances, err := s.QueryWorkflowInstancesByRepoIssue("org/a", 7)
	if err != nil {
		t.Fatalf("QueryWorkflowInstancesByRepoIssue: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}

	transitions, err := s.QueryWorkflowTransitions(id)
	if err != nil {
		t.Fatalf("QueryWorkflowTransitions: %v", err)
	}
	if len(transitions) != 1 || transitions[0].ToState != "developing" {
		t.Fatalf("unexpected transitions: %+v", transitions)
	}

	err = s.AdvanceWorkflowInstance("does/not#1#exist", "a", "b", "agent", time.Now())
	if err == nil {
		t.Fatal("expected ErrWorkflowInstanceNotFound, got nil")
	}
}

func mustInsertEvent(t *testing.T, s Store, e Event) {
	t.Helper()
	if _, err := s.InsertEvent(e); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
}
