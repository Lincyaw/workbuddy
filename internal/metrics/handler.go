package metrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/hooks"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// HookStatsSource is satisfied by *hooks.Dispatcher and exposes the
// per-hook counters needed for `workbuddy_hook_*` metrics. Defining it
// here keeps the metrics package decoupled from a concrete dispatcher.
type HookStatsSource interface {
	Stats() []hooks.HookStats
	OverflowCount() uint64
	DroppedCount() uint64
}

const stuckThreshold = time.Hour

// Source is the narrow, read-only view of persistence required by the metrics
// handler. It is satisfied by *store.Store and exists so this package never
// touches a raw *sql.DB. Keeping the surface small (only aggregates, not
// arbitrary SQL) is the point of issue #145 finding #9.
type Source interface {
	CountEventsByRepoType() ([]store.EventCountByRepoType, error)
	TokenUsageEvents(eventType string) ([]store.TokenUsagePayload, error)
	CountTasksByRepoStatus() ([]store.TaskCountByRepoStatus, error)
	CountWorkersByRepo() ([]store.WorkerCountByRepo, error)
	MaxTransitionCounts() ([]store.TransitionMaxCount, error)
	ListOpenIssueActivity(pendingStatus, runningStatus string) ([]store.IssueActivityRow, error)
}

// Handler serves Prometheus metrics from SQLite state.
type Handler struct {
	src   Source
	evlog *eventlog.EventLogger
	hooks HookStatsSource
	now   func() time.Time
}

// NewHandler returns a handler bound to the provided metrics source. Any type
// satisfying Source can be passed; in production this is *store.Store.
func NewHandler(src Source) *Handler {
	return &Handler{
		src: src,
		now: time.Now,
	}
}

// WithEventLogger attaches an EventLogger whose health is exposed as the
// `workbuddy_eventlog_write_failures_total` and `workbuddy_eventlog_degraded`
// metrics. Passing nil is a no-op. Returns the receiver for chaining.
func (h *Handler) WithEventLogger(ev *eventlog.EventLogger) *Handler {
	h.evlog = ev
	return h
}

// WithHooks attaches a hooks dispatcher whose per-hook stats are exposed as
// `workbuddy_hook_invocations_total`, `workbuddy_hook_duration_seconds`, and
// `workbuddy_hook_overflow_total`. Passing nil is a no-op.
func (h *Handler) WithHooks(src HookStatsSource) *Handler {
	h.hooks = src
	return h
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
	if err := h.writeEventCounters(b); err != nil {
		return err
	}
	if err := h.writeTaskCounters(b); err != nil {
		return err
	}
	if err := h.writeWorkerCounters(b); err != nil {
		return err
	}
	if err := h.writeTransitionMaxCounters(b); err != nil {
		return err
	}
	h.writeEventlogHealth(b)
	h.writeHookMetrics(b)
	return h.writeIssueCounters(b, now)
}

func (h *Handler) writeHookMetrics(b *strings.Builder) {
	if h.hooks == nil {
		return
	}
	stats := h.hooks.Stats()

	_, _ = b.WriteString("# HELP workbuddy_hook_invocations_total Hook invocations by hook and result (success/failure/filtered/disabled/overflow).\n")
	_, _ = b.WriteString("# TYPE workbuddy_hook_invocations_total counter\n")

	type pair struct {
		hook   string
		result string
		count  uint64
	}
	var rows []pair
	for _, s := range stats {
		rows = append(rows,
			pair{s.Name, "success", s.Successes},
			pair{s.Name, "failure", s.Failures},
			pair{s.Name, "filtered", s.Filtered},
			pair{s.Name, "disabled", s.DisabledDrops},
		)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].hook == rows[j].hook {
			return rows[i].result < rows[j].result
		}
		return rows[i].hook < rows[j].hook
	})
	for _, r := range rows {
		_, _ = b.WriteString(fmt.Sprintf(`workbuddy_hook_invocations_total{hook=%s,result=%s} %d`+"\n",
			promLabel(r.hook, "hook"), promLabel(r.result, "result"), r.count))
	}
	// Aggregate overflow as a separate counter — central buffer overflow is
	// hook-agnostic. Per-hook slot drops are surfaced as result=overflow.
	_, _ = b.WriteString("# HELP workbuddy_hook_overflow_total Central dispatcher buffer overflows since process start.\n")
	_, _ = b.WriteString("# TYPE workbuddy_hook_overflow_total counter\n")
	_, _ = b.WriteString(fmt.Sprintf("workbuddy_hook_overflow_total %d\n", h.hooks.OverflowCount()))

	// Per-hook overflow attribution: per-hook slot drops live in
	// DispatcherDroppedCount() in aggregate; we surface that here as a
	// hook-labelled metric where possible. Without per-hook drop tracking we
	// expose the aggregate under hook="*".
	_, _ = b.WriteString(fmt.Sprintf(`workbuddy_hook_invocations_total{hook=%s,result=%s} %d`+"\n",
		promLabel("*", "hook"), promLabel("overflow", "result"), h.hooks.DroppedCount()))

	// Histogram: workbuddy_hook_duration_seconds{hook=...}
	_, _ = b.WriteString("# HELP workbuddy_hook_duration_seconds Histogram of hook action durations in seconds.\n")
	_, _ = b.WriteString("# TYPE workbuddy_hook_duration_seconds histogram\n")
	bounds := hooks.DurationBucketUpperBounds()
	sortedStats := append([]hooks.HookStats(nil), stats...)
	sort.Slice(sortedStats, func(i, j int) bool { return sortedStats[i].Name < sortedStats[j].Name })
	for _, s := range sortedStats {
		hookLabel := promLabel(s.Name, "hook")
		var cumulative uint64
		for i, upper := range bounds {
			cumulative = s.DurationBuckets[i]
			_, _ = b.WriteString(fmt.Sprintf(`workbuddy_hook_duration_seconds_bucket{hook=%s,le="%g"} %d`+"\n",
				hookLabel, upper, cumulative))
			_ = cumulative
		}
		_, _ = b.WriteString(fmt.Sprintf(`workbuddy_hook_duration_seconds_bucket{hook=%s,le="+Inf"} %d`+"\n",
			hookLabel, s.DurationCount))
		_, _ = b.WriteString(fmt.Sprintf(`workbuddy_hook_duration_seconds_sum{hook=%s} %g`+"\n",
			hookLabel, float64(s.DurationSumNs)/1e9))
		_, _ = b.WriteString(fmt.Sprintf(`workbuddy_hook_duration_seconds_count{hook=%s} %d`+"\n",
			hookLabel, s.DurationCount))
	}
}

func (h *Handler) writeEventlogHealth(b *strings.Builder) {
	if h.evlog == nil {
		return
	}
	hs := h.evlog.Health()
	_, _ = b.WriteString("# HELP workbuddy_eventlog_write_failures_total Number of event-log writes that failed since process start.\n")
	_, _ = b.WriteString("# TYPE workbuddy_eventlog_write_failures_total counter\n")
	_, _ = b.WriteString(fmt.Sprintf("workbuddy_eventlog_write_failures_total %d\n", hs.WriteFailures))
	_, _ = b.WriteString("# HELP workbuddy_eventlog_degraded 1 if any event-log write has failed since process start, else 0.\n")
	_, _ = b.WriteString("# TYPE workbuddy_eventlog_degraded gauge\n")
	degraded := 0
	if hs.Degraded {
		degraded = 1
	}
	_, _ = b.WriteString(fmt.Sprintf("workbuddy_eventlog_degraded %d\n", degraded))
}

func (h *Handler) writeEventCounters(b *strings.Builder) error {
	agg, err := h.src.CountEventsByRepoType()
	if err != nil {
		return fmt.Errorf("events query: %w", err)
	}
	metrics := append([]store.EventCountByRepoType(nil), agg...)
	eventCounts := make(map[string]map[string]int64)
	for _, m := range metrics {
		byType, ok := eventCounts[m.Type]
		if !ok {
			byType = make(map[string]int64)
			eventCounts[m.Type] = byType
		}
		byType[m.Repo] = m.Count
	}

	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].Repo == metrics[j].Repo {
			return metrics[i].Type < metrics[j].Type
		}
		return metrics[i].Repo < metrics[j].Repo
	})

	_, _ = b.WriteString("# HELP workbuddy_events_total Total count of lifecycle events by type and repository.\n")
	_, _ = b.WriteString("# TYPE workbuddy_events_total counter\n")
	for _, m := range metrics {
		_, _ = b.WriteString(fmt.Sprintf(`workbuddy_events_total{repo=%s,type=%s} %d`+"\n",
			promLabel(m.Repo, "repo"), promLabel(m.Type, "type"), m.Count))
	}
	h.writeDerivedEventCounter(b, "workbuddy_tasks_dispatched_total", "Total workflow dispatch events.", eventCounts[eventlog.TypeDispatch])
	h.writeDerivedEventCounter(b, "workbuddy_retry_limit_reached_total", "Total retry limit reached events.", eventCounts[eventlog.TypeRetryLimit])
	h.writeDerivedEventCounter(b, "workbuddy_cycle_limit_reached_total", "Total cycle limit reached events.", eventCounts[eventlog.TypeCycleLimitReached])
	h.writeDerivedEventCounter(b, "workbuddy_dependency_blocked_total", "Total events for dispatches blocked by dependency verdict.", eventCounts[eventlog.TypeDispatchBlockedByDependency])

	tokenEvents, err := h.src.TokenUsageEvents(eventlog.TypeTokenUsage)
	if err != nil {
		return fmt.Errorf("token usage query: %w", err)
	}
	tokenTotals := map[string]map[string]int64{}
	var tokenParseErrors int64
	for _, ev := range tokenEvents {
		var usage struct {
			Input  int64 `json:"input"`
			Output int64 `json:"output"`
			Cached int64 `json:"cached"`
			Total  int64 `json:"total"`
		}
		if err := json.Unmarshal([]byte(ev.Payload), &usage); err != nil {
			tokenParseErrors++
			continue
		}
		agg, ok := tokenTotals[ev.Repo]
		if !ok {
			agg = map[string]int64{}
			tokenTotals[ev.Repo] = agg
		}
		agg["input"] += usage.Input
		agg["output"] += usage.Output
		agg["cached"] += usage.Cached
		agg["total"] += usage.Total
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

func (h *Handler) writeTaskCounters(b *strings.Builder) error {
	metrics, err := h.src.CountTasksByRepoStatus()
	if err != nil {
		return fmt.Errorf("tasks query: %w", err)
	}
	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].Repo == metrics[j].Repo {
			return metrics[i].Status < metrics[j].Status
		}
		return metrics[i].Repo < metrics[j].Repo
	})

	_, _ = b.WriteString("# HELP workbuddy_tasks_active Active task count by repo and status.\n")
	_, _ = b.WriteString("# TYPE workbuddy_tasks_active gauge\n")
	for _, m := range metrics {
		_, _ = b.WriteString(fmt.Sprintf(`workbuddy_tasks_active{repo=%s,status=%s} %d`+"\n",
			promLabel(m.Repo, "repo"), promLabel(m.Status, "status"), m.Count))
	}
	return nil
}

func (h *Handler) writeWorkerCounters(b *strings.Builder) error {
	metrics, err := h.src.CountWorkersByRepo()
	if err != nil {
		return fmt.Errorf("workers query: %w", err)
	}
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].Repo < metrics[j].Repo })
	_, _ = b.WriteString("# HELP workbuddy_workers_total Total registered workers by repo.\n")
	_, _ = b.WriteString("# TYPE workbuddy_workers_total gauge\n")
	_, _ = b.WriteString("# HELP workbuddy_workers_online Online workers by repo.\n")
	_, _ = b.WriteString("# TYPE workbuddy_workers_online gauge\n")
	for _, m := range metrics {
		repoLabel := promLabel(m.Repo, "repo")
		_, _ = b.WriteString(fmt.Sprintf("workbuddy_workers_total{repo=%s} %d\n", repoLabel, m.Total))
		_, _ = b.WriteString(fmt.Sprintf("workbuddy_workers_online{repo=%s} %d\n", repoLabel, m.Online))
	}
	return nil
}

func (h *Handler) writeTransitionMaxCounters(b *strings.Builder) error {
	metrics, err := h.src.MaxTransitionCounts()
	if err != nil {
		return fmt.Errorf("transition max query: %w", err)
	}
	sort.Slice(metrics, func(i, j int) bool {
		a, b := metrics[i], metrics[j]
		if a.Repo == b.Repo {
			if a.FromState == b.FromState {
				return a.ToState < b.ToState
			}
			return a.FromState < b.FromState
		}
		return a.Repo < b.Repo
	})

	_, _ = b.WriteString("# HELP workbuddy_transition_max_count Maximum retry count observed for each workflow transition.\n")
	_, _ = b.WriteString("# TYPE workbuddy_transition_max_count gauge\n")
	for _, m := range metrics {
		_, _ = b.WriteString(fmt.Sprintf(`workbuddy_transition_max_count{repo=%s,from=%s,to=%s} %d`+"\n",
			promLabel(m.Repo, "repo"), promLabel(m.FromState, "from"), promLabel(m.ToState, "to"), m.MaxCount))
	}
	return nil
}

func (h *Handler) writeIssueCounters(b *strings.Builder, now time.Time) error {
	rows, err := h.src.ListOpenIssueActivity(store.TaskStatusPending, store.TaskStatusRunning)
	if err != nil {
		return fmt.Errorf("open issue query: %w", err)
	}

	openIssues := map[string]int64{}
	stuckIssues := map[string]int64{}
	for _, row := range rows {
		openIssues[row.Repo]++

		state := inferCurrentState(row.LabelsJSON, row.State)
		if !isIntermediateState(state) {
			continue
		}
		if row.ActiveTaskCount > 0 {
			continue
		}
		if !row.LastEventAt.Valid {
			continue
		}
		lastEventAt, ok := parseEventTimestamp(row.LastEventAt.String)
		if !ok {
			continue
		}
		if now.Sub(lastEventAt) > stuckThreshold {
			stuckIssues[row.Repo]++
		}
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

func promLabel(value, _ string) string {
	return fmt.Sprintf(`"%s"`, escapePrometheusLabelValue(value))
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
