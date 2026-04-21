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

func mustInsertEvent(t *testing.T, s *Store, e Event) {
	t.Helper()
	if _, err := s.InsertEvent(e); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
}
