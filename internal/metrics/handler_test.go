package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestHandler_CollectsPrometheusMetrics(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "workbuddy-metrics.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repo := "owner/repo"
	seedMetricsFixtureForHandler(t, st, repo)

	mux := http.NewServeMux()
	NewHandler(st).Register(mux)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "text/plain; version=0.0.4" {
		t.Fatalf("content type = %q, want %q", got, "text/plain; version=0.0.4")
	}

	body := resp.Body.String()
	for _, want := range []string{
		`workbuddy_events_total{repo="owner/repo",type="completed"}`,
		`workbuddy_events_total{repo="owner/repo",type="dispatch"}`,
		`workbuddy_tokens_total{kind="input",repo="owner/repo"} 10`,
		`workbuddy_tokens_total{kind="output",repo="owner/repo"} 2`,
		`workbuddy_tokens_total{kind="cached",repo="owner/repo"} 1`,
		`workbuddy_tokens_total{kind="total",repo="owner/repo"} 15`,
		`workbuddy_token_parse_errors_total 1`,
		`workbuddy_tasks_active{repo="owner/repo",status="running"} 1`,
		`workbuddy_workers_online{repo="owner/repo"} 1`,
		`workbuddy_workers_total{repo="owner/repo"} 2`,
		`workbuddy_transition_max_count{repo="owner/repo",from="developing",to="reviewing"} 3`,
		`workbuddy_open_issues{repo="owner/repo"} 1`,
		`workbuddy_stuck_issues{repo="owner/repo"} 1`,
		`workbuddy_tasks_dispatched_total{repo="owner/repo"} 1`,
		`workbuddy_retry_limit_reached_total{repo="owner/repo"} 1`,
		`workbuddy_cycle_limit_reached_total{repo="owner/repo"} 1`,
		`workbuddy_dependency_blocked_total{repo="owner/repo"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing metric %q in body:\n%s", want, body)
		}
	}
}

func TestMetricsHandler_ScrapeLatencyUnder5sOn10kEvents(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "workbuddy-metrics.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := bulkInsertCompletedEvents(st, 10_000); err != nil {
		t.Fatalf("bulkInsertCompletedEvents: %v", err)
	}

	mux := http.NewServeMux()
	NewHandler(st).Register(mux)
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	start := time.Now()
	mux.ServeHTTP(resp, req)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("metrics scrape took %s, expected <= 5s", elapsed)
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
}

func bulkInsertCompletedEvents(st *store.Store, count int) error {
	tx, err := st.DB().Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`INSERT INTO events (type, repo, issue_num, payload) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for i := 0; i < count; i++ {
		payload := fmt.Sprintf(`{"i":%q}`, fmt.Sprintf("%d", i))
		if _, err := stmt.Exec(eventlog.TypeCompleted, "owner/repo", i, payload); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func seedMetricsFixtureForHandler(t *testing.T, st *store.Store, repo string) {
	t.Helper()
	logger := eventlog.NewEventLogger(st)

	logger.Log(eventlog.TypeCompleted, repo, 1, map[string]any{"agent": "dev-agent"})
	logger.Log(eventlog.TypeDispatch, repo, 1, map[string]any{"agent": "dev-agent"})
	logger.Log(eventlog.TypeRetryLimit, repo, 1, nil)
	logger.Log(eventlog.TypeCycleLimitReached, repo, 1, nil)
	logger.Log(eventlog.TypeDispatchBlockedByDependency, repo, 1, nil)
	logger.Log(eventlog.TypeTokenUsage, repo, 1, map[string]any{
		"input":  10,
		"output": 2,
		"cached": 1,
		"total":  15,
	})
	if _, err := st.InsertEvent(store.Event{
		Type:     eventlog.TypeTokenUsage,
		Repo:     repo,
		IssueNum: 2,
		Payload:  `not-json`,
	}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := st.InsertWorker(store.WorkerRecord{
		ID:       "worker-online",
		Repo:     repo,
		Roles:    `["dev"]`,
		Hostname: "host-1",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker online: %v", err)
	}
	if err := st.InsertWorker(store.WorkerRecord{
		ID:       "worker-offline",
		Repo:     repo,
		Roles:    `["dev"]`,
		Hostname: "host-2",
		Status:   "offline",
	}); err != nil {
		t.Fatalf("InsertWorker offline: %v", err)
	}
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-running",
		Repo:      repo,
		IssueNum:  11,
		Status:    store.TaskStatusRunning,
		AgentName: "dev-agent",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	max, err := st.IncrementTransition(repo, 1, "developing", "reviewing")
	if err != nil {
		t.Fatalf("IncrementTransition: %v", err)
	}
	if max < 3 {
		if _, err := st.IncrementTransition(repo, 1, "developing", "reviewing"); err != nil {
			t.Fatalf("IncrementTransition: %v", err)
		}
		if _, err := st.IncrementTransition(repo, 1, "developing", "reviewing"); err != nil {
			t.Fatalf("IncrementTransition: %v", err)
		}
	}

	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: 10,
		Labels:   `["status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	eventID, err := st.InsertEvent(store.Event{
		Type:     eventlog.TypeCompleted,
		Repo:     repo,
		IssueNum: 10,
		Payload:  `{"agent":"dev-agent"}`,
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	stale := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := st.DB().Exec(`UPDATE events SET ts = ? WHERE id = ?`, stale, eventID); err != nil {
		t.Fatalf("set stale event ts: %v", err)
	}
}
