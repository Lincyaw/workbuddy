package diagnose

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/store"
)

const (
	SeverityWarn  = "warn"
	SeverityError = "error"

	KindStuckIssue       = "stuck_issue"
	KindMissedRedispatch = "missed_redispatch"
	KindOrphanedTask     = "orphaned_task"
	KindRepeatedFailure  = "repeated_failure"

	stuckThreshold       = time.Hour
	defaultAgentTimeout  = 60 * time.Minute
	defaultOrphanedAfter = 2 * defaultAgentTimeout
)

type Finding struct {
	Kind         string `json:"kind"`
	Repo         string `json:"repo"`
	IssueNum     int    `json:"issue_num"`
	AgentName    string `json:"agent_name,omitempty"`
	Severity     string `json:"severity"`
	Diagnosis    string `json:"diagnosis"`
	SuggestedFix string `json:"suggested_fix"`
	AutoFixable  bool   `json:"auto_fixable"`
}

func Analyze(st *store.Store, repo string, now time.Time) ([]Finding, error) {
	agentTimeouts, err := loadAgentTimeouts("")
	if err != nil {
		return nil, err
	}
	return analyzeWithTimeouts(st, repo, now, agentTimeouts)
}

func analyzeWithTimeouts(st *store.Store, repo string, now time.Time, agentTimeouts map[string]time.Duration) ([]Finding, error) {
	caches, err := listIssueCaches(st, repo)
	if err != nil {
		return nil, err
	}
	events, err := st.QueryEvents(repo)
	if err != nil {
		return nil, fmt.Errorf("diagnose: query events: %w", err)
	}
	tasks, err := st.QueryTasks("")
	if err != nil {
		return nil, fmt.Errorf("diagnose: query tasks: %w", err)
	}

	latestEvent := make(map[string]time.Time)
	for _, event := range events {
		key := issueKey(event.Repo, event.IssueNum)
		if ts, ok := latestEvent[key]; !ok || event.TS.After(ts) {
			latestEvent[key] = event.TS
		}
	}

	findings := make([]Finding, 0)
	for _, cache := range caches {
		state := currentState(cache.Labels, cache.State)
		if !isIntermediateState(state) {
			continue
		}
		active, err := st.HasAnyActiveTask(cache.Repo, cache.IssueNum)
		if err != nil {
			return nil, fmt.Errorf("diagnose: active task lookup for %s#%d: %w", cache.Repo, cache.IssueNum, err)
		}
		depState, err := st.QueryIssueDependencyState(cache.Repo, cache.IssueNum)
		if err != nil {
			return nil, fmt.Errorf("diagnose: dependency state lookup for %s#%d: %w", cache.Repo, cache.IssueNum, err)
		}
		lastEventAt, ok := latestEvent[issueKey(cache.Repo, cache.IssueNum)]
		if ok && !active && now.Sub(lastEventAt) > stuckThreshold {
			findings = append(findings, Finding{
				Kind:         KindStuckIssue,
				Repo:         cache.Repo,
				IssueNum:     cache.IssueNum,
				Severity:     SeverityWarn,
				Diagnosis:    fmt.Sprintf("issue is stuck in %s with no active task for %s", state, now.Sub(lastEventAt).Round(time.Minute)),
				SuggestedFix: "cache-invalidate",
				AutoFixable:  true,
			})
		}
		if depState != nil && depState.Verdict == store.DependencyVerdictReady && !active {
			findings = append(findings, Finding{
				Kind:         KindMissedRedispatch,
				Repo:         cache.Repo,
				IssueNum:     cache.IssueNum,
				Severity:     SeverityWarn,
				Diagnosis:    fmt.Sprintf("dependency verdict is ready but %s has no active task", state),
				SuggestedFix: "cache-invalidate",
				AutoFixable:  true,
			})
		}
	}

	// Build issue-state lookup for the cross-check below. An orphan signal
	// that does NOT flow through worker heartbeat (which keeps UpdatedAt
	// fresh even when the codex child is dead) is: the task is for state X
	// but the issue's labels have already transitioned past X. This catches
	// the "heartbeat-only zombie" class (see #142): the worker's heartbeat
	// goroutine is happily refreshing updated_at while the actual agent
	// work is long done and committed, so the UpdatedAt-based check below
	// can never fire. The label-state cross-check is a non-heartbeat signal.
	//
	// Note: currentState() returns the full label ("status:developing"),
	// but task.State stores the bare workflow-state name ("developing").
	// We strip the "status:" prefix so the two can be compared.
	issueCurrentState := make(map[string]string, len(caches))
	for _, cache := range caches {
		cur := currentState(cache.Labels, cache.State)
		cur = strings.TrimPrefix(cur, "status:")
		if cur == "" {
			continue
		}
		issueCurrentState[issueKey(cache.Repo, cache.IssueNum)] = cur
	}

	for _, task := range tasks {
		if repo != "" && task.Repo != repo {
			continue
		}
		orphanedThreshold := orphanedThresholdForTask(task, agentTimeouts)
		if task.Status == store.TaskStatusRunning && now.Sub(task.UpdatedAt) > orphanedThreshold {
			findings = append(findings, Finding{
				Kind:         KindOrphanedTask,
				Repo:         task.Repo,
				IssueNum:     task.IssueNum,
				AgentName:    task.AgentName,
				Severity:     SeverityError,
				Diagnosis:    fmt.Sprintf("task %s has been running without updates for %s", task.ID, now.Sub(task.UpdatedAt).Round(time.Minute)),
				SuggestedFix: "escalate to operator; inspect worker or restart serve",
			})
			continue
		}
		// Heartbeat-only zombie detection: running task whose dispatch state
		// is no longer the issue's current state. The agent has demonstrably
		// finished (label already flipped) even though the DB row never
		// transitioned off 'running'.
		if task.Status == store.TaskStatusRunning && strings.TrimSpace(task.State) != "" {
			key := issueKey(task.Repo, task.IssueNum)
			if cur, ok := issueCurrentState[key]; ok && cur != "" && cur != task.State {
				findings = append(findings, Finding{
					Kind:      KindOrphanedTask,
					Repo:      task.Repo,
					IssueNum:  task.IssueNum,
					AgentName: task.AgentName,
					Severity:  SeverityError,
					Diagnosis: fmt.Sprintf("task %s (%s) is running for state %q but issue is now in %q — agent already done",
						task.ID, task.AgentName, task.State, cur),
					// Not auto-fixable by cache-invalidate (the default diagnose
					// --fix action): we need to mark the task completed so its
					// worker slot is released. Leave this to the operator; they
					// can verify the dev agent truly succeeded (PR created,
					// label flipped) before unblocking.
					SuggestedFix: "mark task completed so slot is released; restart worker if heartbeat is stuck",
				})
			}
		}
	}

	failCounts := consecutiveFailureCounts(tasks, repo)
	for key, count := range failCounts {
		if count < 3 {
			continue
		}
		repoName, issueNum, agentName := parseFailureKey(key)
		findings = append(findings, Finding{
			Kind:         KindRepeatedFailure,
			Repo:         repoName,
			IssueNum:     issueNum,
			AgentName:    agentName,
			Severity:     SeverityError,
			Diagnosis:    fmt.Sprintf("%s has failed %d times in a row", agentName, count),
			SuggestedFix: "escalate to operator; inspect runtime/session logs",
		})
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Repo != findings[j].Repo {
			return findings[i].Repo < findings[j].Repo
		}
		if findings[i].IssueNum != findings[j].IssueNum {
			return findings[i].IssueNum < findings[j].IssueNum
		}
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity > findings[j].Severity
		}
		return findings[i].Kind < findings[j].Kind
	})
	return findings, nil
}

func loadAgentTimeouts(configDir string) (map[string]time.Duration, error) {
	if strings.TrimSpace(configDir) == "" {
		resolved, err := findConfigDir()
		if err != nil {
			return nil, err
		}
		configDir = resolved
	}
	if configDir == "" {
		return map[string]time.Duration{}, nil
	}
	cfg, _, err := config.LoadConfig(configDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]time.Duration{}, nil
		}
		return nil, fmt.Errorf("diagnose: load config: %w", err)
	}
	timeouts := make(map[string]time.Duration, len(cfg.Agents))
	for name, agent := range cfg.Agents {
		if agent.Timeout > 0 {
			timeouts[name] = agent.Timeout
		}
	}
	return timeouts, nil
}

func findConfigDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("diagnose: getwd: %w", err)
	}
	dir := wd
	for {
		candidate := filepath.Join(dir, ".github", "workbuddy")
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			return candidate, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("diagnose: stat config dir: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

func orphanedThresholdForTask(task store.TaskRecord, agentTimeouts map[string]time.Duration) time.Duration {
	timeout := defaultAgentTimeout
	if d, ok := agentTimeouts[task.AgentName]; ok && d > 0 {
		timeout = d
	}
	return 2 * timeout
}

func listIssueCaches(st *store.Store, repo string) ([]store.IssueCache, error) {
	if repo != "" {
		caches, err := st.ListIssueCaches(repo)
		if err != nil {
			return nil, fmt.Errorf("diagnose: list issue caches: %w", err)
		}
		return caches, nil
	}

	allEvents, err := st.QueryEvents("")
	if err != nil {
		return nil, fmt.Errorf("diagnose: query all events: %w", err)
	}
	repos := make(map[string]struct{})
	for _, event := range allEvents {
		if event.Repo != "" {
			repos[event.Repo] = struct{}{}
		}
	}
	allTasks, err := st.QueryTasks("")
	if err != nil {
		return nil, fmt.Errorf("diagnose: query all tasks: %w", err)
	}
	for _, task := range allTasks {
		if task.Repo != "" {
			repos[task.Repo] = struct{}{}
		}
	}

	caches := make([]store.IssueCache, 0)
	for repoName := range repos {
		rows, err := st.ListIssueCaches(repoName)
		if err != nil {
			return nil, fmt.Errorf("diagnose: list issue caches for %s: %w", repoName, err)
		}
		caches = append(caches, rows...)
	}
	return caches, nil
}

func consecutiveFailureCounts(tasks []store.TaskRecord, repo string) map[string]int {
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
			return tasks[i].ID > tasks[j].ID
		}
		return tasks[i].CreatedAt.After(tasks[j].CreatedAt)
	})
	counts := make(map[string]int)
	closed := make(map[string]struct{})
	for _, task := range tasks {
		if repo != "" && task.Repo != repo {
			continue
		}
		key := fmt.Sprintf("%s|%d|%s", task.Repo, task.IssueNum, task.AgentName)
		if _, done := closed[key]; done {
			continue
		}
		if task.Status == store.TaskStatusFailed {
			counts[key]++
			continue
		}
		closed[key] = struct{}{}
	}
	return counts
}

func parseFailureKey(raw string) (string, int, string) {
	parts := strings.Split(raw, "|")
	if len(parts) != 3 {
		return raw, 0, ""
	}
	issueNum := 0
	fmt.Sscanf(parts[1], "%d", &issueNum)
	return parts[0], issueNum, parts[2]
}

func issueKey(repo string, issueNum int) string {
	return fmt.Sprintf("%s#%d", repo, issueNum)
}

func currentState(labelsJSON, fallback string) string {
	var labels []string
	_ = json.Unmarshal([]byte(labelsJSON), &labels)
	for _, label := range labels {
		if label == "status:developing" || label == "status:reviewing" {
			return label
		}
	}
	return fallback
}

func isIntermediateState(state string) bool {
	return state == "status:developing" || state == "status:reviewing"
}
