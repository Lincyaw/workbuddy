package diagnose

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

const (
	SeverityWarn  = "warn"
	SeverityError = "error"

	KindStuckIssue       = "stuck_issue"
	KindMissedRedispatch = "missed_redispatch"
	KindOrphanedTask     = "orphaned_task"
	KindRepeatedFailure  = "repeated_failure"

	stuckThreshold    = time.Hour
	orphanedThreshold = 60 * time.Minute
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
			return nil, err
		}
		depState, err := st.QueryIssueDependencyState(cache.Repo, cache.IssueNum)
		if err != nil {
			return nil, err
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

	for _, task := range tasks {
		if repo != "" && task.Repo != repo {
			continue
		}
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
