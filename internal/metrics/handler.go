package metrics

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

const stuckThreshold = time.Hour

// Handler serves Prometheus metrics from SQLite state.
type Handler struct {
	store *store.Store
	now   func() time.Time
}

// NewHandler returns a handler bound to the provided store.
func NewHandler(st *store.Store) *Handler {
	return &Handler{
		store: st,
		now:   time.Now,
	}
}

// Register registers GET /metrics.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/metrics", h.handleMetrics)
}

func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	now := h.now().UTC()
	var b strings.Builder
	if err := h.writeMetrics(&b, now); err != nil {
		http.Error(w, fmt.Sprintf("render metrics: %v", err), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(b.String()))
}

func (h *Handler) writeMetrics(b *strings.Builder, now time.Time) error {
	db := h.store.DB()
	if err := h.writeEventCounters(b, db); err != nil {
		return err
	}
	if err := h.writeTaskCounters(b, db); err != nil {
		return err
	}
	if err := h.writeWorkerCounters(b, db); err != nil {
		return err
	}
	if err := h.writeTransitionMaxCounters(b, db); err != nil {
		return err
	}
	return h.writeIssueCounters(b, db, now)
}

func (h *Handler) writeEventCounters(b *strings.Builder, db *sql.DB) error {
	rows, err := db.Query(`SELECT COALESCE(repo, ''), COALESCE(type, ''), COUNT(1) FROM events GROUP BY repo, type`)
	if err != nil {
		return fmt.Errorf("events query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type eventMetric struct {
		repo string
		typ  string
		cnt  int64
	}
	metrics := make([]eventMetric, 0)
	eventCounts := make(map[string]map[string]int64)
	for rows.Next() {
		var m eventMetric
		if err := rows.Scan(&m.repo, &m.typ, &m.cnt); err != nil {
			return fmt.Errorf("scan events: %w", err)
		}
		metrics = append(metrics, m)
		byType, ok := eventCounts[m.typ]
		if !ok {
			byType = make(map[string]int64)
			eventCounts[m.typ] = byType
		}
		byType[m.repo] = m.cnt
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan events rows: %w", err)
	}

	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].repo == metrics[j].repo {
			return metrics[i].typ < metrics[j].typ
		}
		return metrics[i].repo < metrics[j].repo
	})

	_, _ = b.WriteString("# HELP workbuddy_events_total Total count of lifecycle events by type and repository.\n")
	_, _ = b.WriteString("# TYPE workbuddy_events_total counter\n")
	for _, m := range metrics {
		_, _ = b.WriteString(fmt.Sprintf(`workbuddy_events_total{repo=%s,type=%s} %d`+"\n",
			promLabel(m.repo, "repo"), promLabel(m.typ, "type"), m.cnt))
	}
	h.writeDerivedEventCounter(b, "workbuddy_tasks_dispatched_total", "Total workflow dispatch events.", eventCounts[eventlog.TypeDispatch])
	h.writeDerivedEventCounter(b, "workbuddy_retry_limit_reached_total", "Total retry limit reached events.", eventCounts[eventlog.TypeRetryLimit])
	h.writeDerivedEventCounter(b, "workbuddy_cycle_limit_reached_total", "Total cycle limit reached events.", eventCounts[eventlog.TypeCycleLimitReached])
	h.writeDerivedEventCounter(b, "workbuddy_dependency_blocked_total", "Total events for dispatches blocked by dependency verdict.", eventCounts[eventlog.TypeDispatchBlockedByDependency])

	tokenTotals := map[string]map[string]int64{}
	var tokenParseErrors int64
	tokenRows, err := db.Query(`SELECT COALESCE(repo, ''), COALESCE(payload, '') FROM events WHERE type = ?`, eventlog.TypeTokenUsage)
	if err != nil {
		return fmt.Errorf("token usage query: %w", err)
	}
	defer func() { _ = tokenRows.Close() }()
	for tokenRows.Next() {
		var repo, rawPayload string
		if err := tokenRows.Scan(&repo, &rawPayload); err != nil {
			return fmt.Errorf("scan token usage: %w", err)
		}
		var usage struct {
			Input  int64 `json:"input"`
			Output int64 `json:"output"`
			Cached int64 `json:"cached"`
			Total  int64 `json:"total"`
		}
		if err := json.Unmarshal([]byte(rawPayload), &usage); err != nil {
			tokenParseErrors++
			continue
		}

		agg, ok := tokenTotals[repo]
		if !ok {
			agg = map[string]int64{}
			tokenTotals[repo] = agg
		}
		agg["input"] += usage.Input
		agg["output"] += usage.Output
		agg["cached"] += usage.Cached
		agg["total"] += usage.Total
	}
	if err := tokenRows.Err(); err != nil {
		return fmt.Errorf("scan token usage rows: %w", err)
	}

	_, _ = b.WriteString("# HELP workbuddy_token_parse_errors_total Number of malformed token usage payloads.\n")
	_, _ = b.WriteString("# TYPE workbuddy_token_parse_errors_total counter\n")
	_, _ = b.WriteString(fmt.Sprintf("workbuddy_token_parse_errors_total %d\n", tokenParseErrors))

	_, _ = b.WriteString("# HELP workbuddy_tokens_total Total token usage by repo and kind.\n")
	_, _ = b.WriteString("# TYPE workbuddy_tokens_total counter\n")
	repos := make([]string, 0, len(tokenTotals))
	for repo := range tokenTotals {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		for _, kind := range []string{"cached", "input", "output", "total"} {
			_, _ = b.WriteString(fmt.Sprintf(`workbuddy_tokens_total{kind=%s,repo=%s} %d`+"\n",
				promLabel(kind, "kind"), promLabel(repo, "repo"), tokenTotals[repo][kind]))
		}
	}
	return nil
}

func (h *Handler) writeDerivedEventCounter(b *strings.Builder, metricName, helpText string, byRepo map[string]int64) {
	_, _ = b.WriteString(fmt.Sprintf("# HELP %s %s\n", metricName, helpText))
	_, _ = b.WriteString(fmt.Sprintf("# TYPE %s counter\n", metricName))
	if len(byRepo) == 0 {
		return
	}

	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		_, _ = b.WriteString(fmt.Sprintf("%s{repo=%s} %d\n",
			metricName, promLabel(repo, "repo"), byRepo[repo]))
	}
}

func (h *Handler) writeTaskCounters(b *strings.Builder, db *sql.DB) error {
	rows, err := db.Query(`SELECT COALESCE(repo, ''), COALESCE(status, ''), COUNT(1) FROM task_queue GROUP BY repo, status`)
	if err != nil {
		return fmt.Errorf("tasks query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type metric struct {
		repo   string
		status string
		count  int64
	}
	metrics := make([]metric, 0)
	for rows.Next() {
		var m metric
		if err := rows.Scan(&m.repo, &m.status, &m.count); err != nil {
			return fmt.Errorf("scan tasks: %w", err)
		}
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan task rows: %w", err)
	}

	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].repo == metrics[j].repo {
			return metrics[i].status < metrics[j].status
		}
		return metrics[i].repo < metrics[j].repo
	})

	_, _ = b.WriteString("# HELP workbuddy_tasks_active Active task count by repo and status.\n")
	_, _ = b.WriteString("# TYPE workbuddy_tasks_active gauge\n")
	for _, m := range metrics {
		_, _ = b.WriteString(fmt.Sprintf(`workbuddy_tasks_active{repo=%s,status=%s} %d`+"\n",
			promLabel(m.repo, "repo"), promLabel(m.status, "status"), m.count))
	}
	return nil
}

func (h *Handler) writeWorkerCounters(b *strings.Builder, db *sql.DB) error {
	rows, err := db.Query(`
		SELECT COALESCE(repo, ''), COUNT(1),
		       SUM(CASE WHEN status = 'online' THEN 1 ELSE 0 END)
		FROM workers
		GROUP BY repo`)
	if err != nil {
		return fmt.Errorf("workers query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type metric struct {
		repo   string
		total  int64
		online int64
	}
	metrics := make([]metric, 0)
	for rows.Next() {
		var m metric
		if err := rows.Scan(&m.repo, &m.total, &m.online); err != nil {
			return fmt.Errorf("scan workers: %w", err)
		}
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan worker rows: %w", err)
	}

	sort.Slice(metrics, func(i, j int) bool { return metrics[i].repo < metrics[j].repo })
	_, _ = b.WriteString("# HELP workbuddy_workers_total Total registered workers by repo.\n")
	_, _ = b.WriteString("# TYPE workbuddy_workers_total gauge\n")
	_, _ = b.WriteString("# HELP workbuddy_workers_online Online workers by repo.\n")
	_, _ = b.WriteString("# TYPE workbuddy_workers_online gauge\n")
	for _, m := range metrics {
		repoLabel := promLabel(m.repo, "repo")
		_, _ = b.WriteString(fmt.Sprintf("workbuddy_workers_total{repo=%s} %d\n", repoLabel, m.total))
		_, _ = b.WriteString(fmt.Sprintf("workbuddy_workers_online{repo=%s} %d\n", repoLabel, m.online))
	}
	return nil
}

func (h *Handler) writeTransitionMaxCounters(b *strings.Builder, db *sql.DB) error {
	rows, err := db.Query(`SELECT COALESCE(repo, ''), COALESCE(from_state, ''), COALESCE(to_state, ''), MAX(count) FROM transition_counts GROUP BY repo, from_state, to_state`)
	if err != nil {
		return fmt.Errorf("transition max query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type metric struct {
		repo      string
		fromState string
		toState   string
		maxCount  int64
	}
	metrics := make([]metric, 0)
	for rows.Next() {
		var m metric
		if err := rows.Scan(&m.repo, &m.fromState, &m.toState, &m.maxCount); err != nil {
			return fmt.Errorf("scan transition max: %w", err)
		}
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan transition max rows: %w", err)
	}

	sort.Slice(metrics, func(i, j int) bool {
		a, b := metrics[i], metrics[j]
		if a.repo == b.repo {
			if a.fromState == b.fromState {
				return a.toState < b.toState
			}
			return a.fromState < b.fromState
		}
		return a.repo < b.repo
	})

	_, _ = b.WriteString("# HELP workbuddy_transition_max_count Maximum retry count observed for each workflow transition.\n")
	_, _ = b.WriteString("# TYPE workbuddy_transition_max_count gauge\n")
	for _, m := range metrics {
		_, _ = b.WriteString(fmt.Sprintf(`workbuddy_transition_max_count{repo=%s,from=%s,to=%s} %d`+"\n",
			promLabel(m.repo, "repo"), promLabel(m.fromState, "from"), promLabel(m.toState, "to"), m.maxCount))
	}
	return nil
}

func (h *Handler) writeIssueCounters(b *strings.Builder, db *sql.DB, now time.Time) error {
	rows, err := db.Query(`
		SELECT
			ic.repo,
			ic.issue_num,
			ic.labels,
			COALESCE(ic.state, ''),
			(
				SELECT MAX(e.ts) FROM events e
				WHERE e.repo = ic.repo AND e.issue_num = ic.issue_num
			),
			(
				SELECT COUNT(1) FROM task_queue tq
				WHERE tq.repo = ic.repo AND tq.issue_num = ic.issue_num
					AND tq.status IN (?, ?)
			)
		FROM issue_cache ic
		WHERE (ic.state = 'open' OR ic.state IS NULL)
			AND (ic.state IS NULL OR ic.state NOT LIKE 'pr:%')`,
		store.TaskStatusPending, store.TaskStatusRunning)
	if err != nil {
		return fmt.Errorf("open issue query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	openIssues := map[string]int64{}
	stuckIssues := map[string]int64{}
	for rows.Next() {
		var repo, labelsJSON, issueState string
		var issueNum int
		var rawLastEvent sql.NullString
		var activeTaskCount int
		if err := rows.Scan(&repo, &issueNum, &labelsJSON, &issueState, &rawLastEvent, &activeTaskCount); err != nil {
			return fmt.Errorf("scan issue row: %w", err)
		}
		openIssues[repo]++

		_ = issueNum
		state := inferCurrentState(labelsJSON, issueState)
		if !isIntermediateState(state) {
			continue
		}
		if activeTaskCount > 0 {
			continue
		}
		if !rawLastEvent.Valid {
			continue
		}
		lastEventAt, ok := parseEventTimestamp(rawLastEvent.String)
		if !ok {
			continue
		}
		if now.Sub(lastEventAt) > stuckThreshold {
			stuckIssues[repo]++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan issue rows: %w", err)
	}

	repos := make([]string, 0, len(openIssues))
	for repo := range openIssues {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	_, _ = b.WriteString("# HELP workbuddy_open_issues Open issues in issue cache.\n")
	_, _ = b.WriteString("# TYPE workbuddy_open_issues gauge\n")
	_, _ = b.WriteString("# HELP workbuddy_stuck_issues Stuck open intermediate-state issues with no active task and no recent event.\n")
	_, _ = b.WriteString("# TYPE workbuddy_stuck_issues gauge\n")
	for _, repo := range repos {
		repoLabel := promLabel(repo, "repo")
		_, _ = b.WriteString(fmt.Sprintf("workbuddy_open_issues{repo=%s} %d\n", repoLabel, openIssues[repo]))
		_, _ = b.WriteString(fmt.Sprintf("workbuddy_stuck_issues{repo=%s} %d\n", repoLabel, stuckIssues[repo]))
	}
	return nil
}

func promLabel(value, key string) string {
	_ = key
	return strconv.Quote(escapePrometheusLabelValue(value))
}

func parseEventTimestamp(raw string) (time.Time, bool) {
	return store.ParseTimestamp(raw, "event.ts")
}

func inferCurrentState(labelsJSON, issueState string) string {
	var labels []string
	_ = json.Unmarshal([]byte(labelsJSON), &labels)
	for _, label := range labels {
		if strings.HasPrefix(label, "status:") {
			return label
		}
	}
	return issueState
}

func isIntermediateState(state string) bool {
	switch state {
	case "", "closed", "status:blocked", "status:done", "status:failed":
		return false
	default:
		return strings.HasPrefix(state, "status:")
	}
}

func escapePrometheusLabelValue(raw string) string {
	return strings.NewReplacer(
		`"`, `\"`,
		"\\", `\\`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	).Replace(raw)
}
