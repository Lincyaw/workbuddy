package operator

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

const (
	KindLeaseExpired          = "lease_expired"
	KindTaskStuck             = "task_stuck"
	KindMissingLabel          = "missing_label"
	KindCacheStale            = "cache_stale"
	KindWorkerMissing         = "worker_missing"
	KindCoordinatorRestartGap = "coordinator_restart_gap"

	SeverityInfo  = "info"
	SeverityWarn  = "warn"
	SeverityError = "error"

	leaseExpiredGrace = 30 * time.Second
	taskStuckAfter    = 10 * time.Minute
	missingLabelAfter = 5 * time.Minute
)

// Alert is the structured anomaly emitted by the operator detector.
type Alert struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Severity string         `json:"severity"`
	Ts       time.Time      `json:"ts"`
	Resource map[string]any `json:"resource,omitempty"`
	Detail   string         `json:"detail"`
	Snapshot map[string]any `json:"snapshot,omitempty"`
}

// ProcessInspector reports how many codex processes are live on the host.
type ProcessInspector interface {
	CodexProcessCount(ctx context.Context) (int, error)
}

type DetectorOptions struct {
	Store                   *store.Store
	Config                  config.OperatorConfig
	AlertBus                *alertbus.Bus
	DefaultRepo             string
	DefaultPollInterval     time.Duration
	WorkerHeartbeatInterval time.Duration
	ProcessInspector        ProcessInspector
	Now                     func() time.Time
}

// Detector periodically scans coordinator state and emits persisted alerts.
type Detector struct {
	store                   *store.Store
	cfg                     config.OperatorConfig
	alertBus                *alertbus.Bus
	defaultRepo             string
	defaultPollInterval     time.Duration
	workerHeartbeatInterval time.Duration
	processInspector        ProcessInspector
	now                     func() time.Time
}

type hostProcessInspector struct{}

type repoContext struct {
	repo         string
	pollInterval time.Duration
}

func NewDetector(opts DetectorOptions) *Detector {
	cfg := opts.Config
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 60 * time.Second
	}
	if cfg.DedupWindow <= 0 {
		cfg.DedupWindow = 5 * time.Minute
	}
	if strings.TrimSpace(cfg.InboxDir) == "" {
		cfg.InboxDir = "~/.workbuddy/operator/inbox"
	}
	if opts.WorkerHeartbeatInterval <= 0 {
		opts.WorkerHeartbeatInterval = 15 * time.Second
	}
	if opts.ProcessInspector == nil {
		opts.ProcessInspector = hostProcessInspector{}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.DefaultPollInterval <= 0 {
		opts.DefaultPollInterval = 30 * time.Second
	}
	return &Detector{
		store:                   opts.Store,
		cfg:                     cfg,
		alertBus:                opts.AlertBus,
		defaultRepo:             strings.TrimSpace(opts.DefaultRepo),
		defaultPollInterval:     opts.DefaultPollInterval,
		workerHeartbeatInterval: opts.WorkerHeartbeatInterval,
		processInspector:        opts.ProcessInspector,
		now:                     opts.Now,
	}
}

func (d *Detector) Run(ctx context.Context) error {
	if d == nil || d.store == nil || !d.cfg.Enabled {
		return nil
	}
	if _, err := d.runPass(ctx, true); err != nil {
		return err
	}
	ticker := time.NewTicker(d.cfg.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := d.runPass(ctx, false); err != nil {
				return err
			}
		}
	}
}

func (d *Detector) runPass(ctx context.Context, startup bool) ([]Alert, error) {
	now := d.now().UTC()
	repos, err := d.repoContexts()
	if err != nil {
		return nil, err
	}

	codexCount := -1
	alerts := make([]Alert, 0)
	for _, repoCtx := range repos {
		repoAlerts, countOut, err := d.detectRepoAlerts(ctx, repoCtx, now, startup, codexCount)
		if err != nil {
			return nil, err
		}
		codexCount = countOut
		alerts = append(alerts, repoAlerts...)
	}

	emitted := make([]Alert, 0, len(alerts))
	for _, alert := range alerts {
		ok, err := d.emit(alert)
		if err != nil {
			return emitted, err
		}
		if ok {
			emitted = append(emitted, alert)
		}
	}
	return emitted, nil
}

func (d *Detector) repoContexts() ([]repoContext, error) {
	contexts := make(map[string]repoContext)
	if d.defaultRepo != "" {
		contexts[d.defaultRepo] = repoContext{repo: d.defaultRepo, pollInterval: d.defaultPollInterval}
	}
	registrations, err := d.store.ListRepoRegistrations()
	if err != nil {
		return nil, fmt.Errorf("operator: list repo registrations: %w", err)
	}
	for _, rec := range registrations {
		repo := strings.TrimSpace(rec.Repo)
		if repo == "" {
			continue
		}
		contexts[repo] = repoContext{repo: repo, pollInterval: d.defaultPollInterval}
	}
	out := make([]repoContext, 0, len(contexts))
	for _, ctx := range contexts {
		out = append(out, ctx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].repo < out[j].repo })
	return out, nil
}

func (d *Detector) detectRepoAlerts(ctx context.Context, repoCtx repoContext, now time.Time, startup bool, codexCount int) ([]Alert, int, error) {
	repo := repoCtx.repo
	caches, err := d.store.ListIssueCaches(repo)
	if err != nil {
		return nil, codexCount, fmt.Errorf("operator: list issue caches for %s: %w", repo, err)
	}
	tasks, err := d.store.QueryTasksFiltered(store.TaskFilter{Repo: repo})
	if err != nil {
		return nil, codexCount, fmt.Errorf("operator: list tasks for %s: %w", repo, err)
	}
	workers, err := d.store.QueryWorkers(repo)
	if err != nil {
		return nil, codexCount, fmt.Errorf("operator: list workers for %s: %w", repo, err)
	}

	hasOnlineWorker := false
	for _, worker := range workers {
		if worker.Status == "online" {
			hasOnlineWorker = true
			break
		}
	}

	alerts := make([]Alert, 0)
	for _, task := range tasks {
		switch {
		case task.Status == store.TaskStatusRunning && !task.LeaseExpiresAt.IsZero():
			if now.After(task.LeaseExpiresAt.Add(leaseExpiredGrace)) {
				if codexCount < 0 {
					count, err := d.processInspector.CodexProcessCount(ctx)
					if err != nil {
						return nil, codexCount, fmt.Errorf("operator: count codex processes: %w", err)
					}
					codexCount = count
				}
				if codexCount == 0 {
					alerts = append(alerts, d.newAlert(now, KindLeaseExpired, SeverityWarn, map[string]any{
						"repo":      repo,
						"issue_num": task.IssueNum,
						"task_id":   task.ID,
					}, fmt.Sprintf("task %s lease expired at %s and no codex process is running", task.ID, task.LeaseExpiresAt.Format(time.RFC3339)), map[string]any{
						"status":           task.Status,
						"worker_id":        task.WorkerID,
						"lease_expires_at": task.LeaseExpiresAt.Format(time.RFC3339),
					}))
				}
			}
		case task.Status == store.TaskStatusPending && now.Sub(task.CreatedAt) > taskStuckAfter && !hasOnlineWorker:
			alerts = append(alerts, d.newAlert(now, KindTaskStuck, SeverityWarn, map[string]any{
				"repo":      repo,
				"issue_num": task.IssueNum,
				"task_id":   task.ID,
			}, fmt.Sprintf("pending task %s has been stuck since %s and repo has no online worker", task.ID, task.CreatedAt.Format(time.RFC3339)), map[string]any{
				"created_at": task.CreatedAt.Format(time.RFC3339),
				"status":     task.Status,
			}))
		}
	}

	for _, cache := range caches {
		labels := decodeLabels(cache.Labels)
		hasWorkbuddy := containsLabel(labels, "workbuddy")
		hasStatus := firstStatusLabel(labels) != ""
		if hasWorkbuddy && !hasStatus && now.Sub(cache.UpdatedAt) > missingLabelAfter {
			alerts = append(alerts, d.newAlert(now, KindMissingLabel, SeverityInfo, map[string]any{
				"repo":      repo,
				"issue_num": cache.IssueNum,
			}, "issue has workbuddy label but no status:* label for more than 5 minutes", map[string]any{
				"labels":      labels,
				"updated_at":  cache.UpdatedAt.Format(time.RFC3339),
				"issue_state": cache.State,
			}))
		}
		if repoCtx.pollInterval > 0 && now.Sub(cache.UpdatedAt) > 2*repoCtx.pollInterval {
			alerts = append(alerts, d.newAlert(now, KindCacheStale, SeverityWarn, map[string]any{
				"repo":      repo,
				"issue_num": cache.IssueNum,
			}, fmt.Sprintf("issue cache has not been refreshed for %s", now.Sub(cache.UpdatedAt).Round(time.Second)), map[string]any{
				"updated_at":    cache.UpdatedAt.Format(time.RFC3339),
				"poll_interval": repoCtx.pollInterval.String(),
			}))
		}
		if startup {
			state := inferCurrentState(labels, cache.State)
			if isIntermediateState(state) {
				count, err := d.countTasksForIssue(repo, cache.IssueNum)
				if err != nil {
					return nil, codexCount, err
				}
				if count == 0 {
					alerts = append(alerts, d.newAlert(now, KindCoordinatorRestartGap, SeverityWarn, map[string]any{
						"repo":      repo,
						"issue_num": cache.IssueNum,
					}, "issue has an active status label but no corresponding task rows after coordinator startup", map[string]any{
						"labels":      labels,
						"updated_at":  cache.UpdatedAt.Format(time.RFC3339),
						"issue_state": cache.State,
					}))
				}
			}
		}
	}

	for _, worker := range workers {
		if worker.Status != "online" {
			continue
		}
		if now.Sub(worker.LastHeartbeat) > 3*d.workerHeartbeatInterval {
			alerts = append(alerts, d.newAlert(now, KindWorkerMissing, SeverityError, map[string]any{
				"repo":      repo,
				"worker_id": worker.ID,
			}, fmt.Sprintf("worker heartbeat is stale by %s", now.Sub(worker.LastHeartbeat).Round(time.Second)), map[string]any{
				"last_heartbeat":      worker.LastHeartbeat.Format(time.RFC3339),
				"heartbeat_interval":  d.workerHeartbeatInterval.String(),
				"registered_at":       worker.RegisteredAt.Format(time.RFC3339),
				"worker_status":       worker.Status,
				"worker_primary_repo": worker.Repo,
			}))
		}
	}

	return alerts, codexCount, nil
}

func (d *Detector) countTasksForIssue(repo string, issueNum int) (int, error) {
	var count int
	err := d.store.DB().QueryRow(`SELECT COUNT(1) FROM task_queue WHERE repo = ? AND issue_num = ?`, repo, issueNum).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("operator: count tasks for %s#%d: %w", repo, issueNum, err)
	}
	return count, nil
}

func (d *Detector) emit(alert Alert) (bool, error) {
	suppressed, err := d.isDuplicate(alert)
	if err != nil {
		return false, err
	}
	if suppressed {
		return false, nil
	}

	payload, err := json.Marshal(alert)
	if err != nil {
		return false, fmt.Errorf("operator: marshal alert: %w", err)
	}
	repo, issueNum := alertRepoIssue(alert)
	if _, err := d.store.InsertEvent(store.Event{
		Type:     eventlog.TypeAlert,
		Repo:     repo,
		IssueNum: issueNum,
		Payload:  string(payload),
	}); err != nil {
		return false, fmt.Errorf("operator: persist alert: %w", err)
	}
	if err := d.writeInbox(alert, payload); err != nil {
		return false, err
	}
	if d.alertBus != nil {
		d.alertBus.Publish(alertbus.AlertEvent{
			Kind:      alert.Kind,
			Severity:  toBusSeverity(alert.Severity),
			Repo:      repo,
			IssueNum:  issueNum,
			Timestamp: alert.Ts.Unix(),
			Payload: map[string]any{
				"alert_id": alert.ID,
				"detail":   alert.Detail,
				"resource": alert.Resource,
				"snapshot": alert.Snapshot,
			},
		})
	}
	return true, nil
}

func (d *Detector) isDuplicate(candidate Alert) (bool, error) {
	rows, err := d.store.DB().Query(
		`SELECT payload FROM events WHERE type = ? ORDER BY id DESC LIMIT 256`,
		eventlog.TypeAlert,
	)
	if err != nil {
		return false, fmt.Errorf("operator: query recent alerts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	wantKey := candidate.Kind + "|" + stableResourceKey(candidate.Resource)
	windowStart := candidate.Ts.Add(-d.cfg.DedupWindow)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return false, fmt.Errorf("operator: scan recent alert: %w", err)
		}
		var existing Alert
		if err := json.Unmarshal([]byte(payload), &existing); err != nil {
			continue
		}
		if existing.Ts.Before(windowStart) {
			continue
		}
		if existing.Kind+"|"+stableResourceKey(existing.Resource) == wantKey {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (d *Detector) writeInbox(alert Alert, payload []byte) error {
	inboxDir, err := expandPath(d.cfg.InboxDir)
	if err != nil {
		return fmt.Errorf("operator: resolve inbox dir: %w", err)
	}
	if err := os.MkdirAll(inboxDir, 0o700); err != nil {
		return fmt.Errorf("operator: create inbox dir: %w", err)
	}
	if err := os.Chmod(inboxDir, 0o700); err != nil {
		return fmt.Errorf("operator: chmod inbox dir: %w", err)
	}
	path := filepath.Join(inboxDir, alert.ID+".json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("operator: write inbox alert: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("operator: chmod inbox alert: %w", err)
	}
	return nil
}

func (d *Detector) newAlert(now time.Time, kind, severity string, resource map[string]any, detail string, snapshot map[string]any) Alert {
	ts := now.UTC()
	return Alert{
		ID:       alertID(ts, kind, resource),
		Kind:     kind,
		Severity: severity,
		Ts:       ts,
		Resource: resource,
		Detail:   detail,
		Snapshot: snapshot,
	}
}

func alertID(ts time.Time, kind string, resource map[string]any) string {
	hash := sha1.Sum([]byte(kind + "|" + stableResourceKey(resource)))
	return fmt.Sprintf("%s-%s-%s", ts.Format("20060102T150405Z"), kind, hex.EncodeToString(hash[:])[:8])
}

func stableResourceKey(resource map[string]any) string {
	if len(resource) == 0 {
		return ""
	}
	keys := make([]string, 0, len(resource))
	for key := range resource {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+canonicalValue(resource[key]))
	}
	return strings.Join(parts, ",")
}

func alertRepoIssue(alert Alert) (string, int) {
	repo, _ := alert.Resource["repo"].(string)
	return repo, intValue(alert.Resource["issue_num"])
}

func toBusSeverity(severity string) alertbus.Severity {
	switch severity {
	case SeverityInfo:
		return alertbus.SeverityInfo
	case SeverityError:
		return alertbus.SeverityError
	default:
		return alertbus.SeverityWarn
	}
}

func inferCurrentState(labels []string, issueState string) string {
	if status := firstStatusLabel(labels); status != "" {
		return status
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

func firstStatusLabel(labels []string) string {
	for _, label := range labels {
		if strings.HasPrefix(label, "status:") {
			return label
		}
	}
	return ""
}

func containsLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func decodeLabels(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var labels []string
	_ = json.Unmarshal([]byte(raw), &labels)
	return labels
}

func intValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	default:
		return 0
	}
}

func canonicalValue(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case float64:
		return strconv.FormatInt(int64(value), 10)
	case json.Number:
		return value.String()
	case bool:
		if value {
			return "true"
		}
		return "false"
	case []string:
		return strings.Join(value, "|")
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			parts = append(parts, canonicalValue(item))
		}
		return strings.Join(parts, "|")
	case map[string]any:
		return stableResourceKey(value)
	default:
		return fmt.Sprint(value)
	}
}

func expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Clean(path), nil
}

func (hostProcessInspector) CodexProcessCount(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "ps", "-eo", "command=").Output()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(strings.ToLower(line), "codex") {
			count++
		}
	}
	return count, nil
}
