package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	recoverpkg "github.com/Lincyaw/workbuddy/internal/recover"
	"github.com/Lincyaw/workbuddy/internal/store"
)

const (
	SeverityWarn  = "warn"
	SeverityError = "error"

	KindStuckIssue       = "stuck_issue"
	KindMissedRedispatch = "missed_redispatch"
	KindOrphanedTask     = "orphaned_task"
	KindRepeatedFailure  = "repeated_failure"
	// KindPipelineHazard is emitted for issues that the coordinator has
	// flagged in issue_pipeline_hazards (REQ #255): configuration-
	// incompleteness conditions that cause silent dispatch skips.
	KindPipelineHazard = "pipeline_hazard"

	stuckThreshold       = time.Hour
	defaultAgentTimeout  = 60 * time.Minute
	defaultIdleThreshold = 10 * time.Minute
	defaultOrphanedAfter = 2 * defaultAgentTimeout
	noChildGracePeriod   = 2 * time.Minute
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
	TaskID       string `json:"-"`
	FixAction    string `json:"-"`
}

func Analyze(st *store.Store, repo string, now time.Time) ([]Finding, error) {
	cfg, err := loadDiagnoseConfig("")
	if err != nil {
		return nil, err
	}
	return analyzeWithConfig(st, repo, now, cfg)
}

type diagnoseConfig struct {
	AgentTimeouts map[string]time.Duration
	IdleThreshold time.Duration
}

type taskProcess struct {
	PID     int
	Base    string
	Command string
	CWD     string
	Elapsed time.Duration
}

var listTaskProcesses = func() ([]taskProcess, error) {
	cmd := exec.CommandContext(context.Background(), "ps", "-eo", "pid=,ppid=,etimes=,args=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("diagnose: ps: %s: %w", strings.TrimSpace(string(out)), err)
	}
	parsed, err := recoverpkg.ParseProcessList(string(out))
	if err != nil {
		return nil, err
	}
	processes := make([]taskProcess, 0, len(parsed))
	for _, proc := range parsed {
		if len(proc.Args) == 0 {
			continue
		}
		base := filepath.Base(proc.Args[0])
		switch base {
		case "codex", "claude":
		default:
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", fmt.Sprintf("%d", proc.PID), "cwd"))
		if err != nil {
			continue
		}
		processes = append(processes, taskProcess{
			PID:     proc.PID,
			Base:    base,
			Command: proc.Command,
			CWD:     cwd,
			Elapsed: time.Duration(proc.ElapsedSeconds) * time.Second,
		})
	}
	return processes, nil
}

var statPath = os.Stat

func analyzeWithConfig(st *store.Store, repo string, now time.Time, cfg diagnoseConfig) ([]Finding, error) {
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
	processes, err := listTaskProcesses()
	if err != nil {
		return nil, err
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
		orphanedThreshold := orphanedThresholdForTask(task, cfg.AgentTimeouts)
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
		if task.Status != store.TaskStatusRunning {
			continue
		}
		worktreeRoot, sessionIdle, sessionStale, err := taskSessionSignal(st, task, now, cfg.IdleThreshold)
		if err != nil {
			return nil, err
		}
		advanced, cur := taskAdvancedPastIssueState(task, issueCurrentState)
		if sessionStale {
			findings = append(findings, Finding{
				Kind:         KindOrphanedTask,
				Repo:         task.Repo,
				IssueNum:     task.IssueNum,
				AgentName:    task.AgentName,
				Severity:     SeverityError,
				Diagnosis:    fmt.Sprintf("heartbeat-only zombie (session log static for %s)", sessionIdle.Round(time.Minute)),
				SuggestedFix: orphanedTaskSuggestedFix(advanced),
				AutoFixable:  true,
				TaskID:       task.ID,
				FixAction:    orphanedTaskFixAction(advanced),
			})
			continue
		}
		if taskMissingChildProcess(task, worktreeRoot, now, processes) {
			findings = append(findings, Finding{
				Kind:         KindOrphanedTask,
				Repo:         task.Repo,
				IssueNum:     task.IssueNum,
				AgentName:    task.AgentName,
				Severity:     SeverityError,
				Diagnosis:    "heartbeat-only zombie (no child process)",
				SuggestedFix: orphanedTaskSuggestedFix(advanced),
				AutoFixable:  true,
				TaskID:       task.ID,
				FixAction:    orphanedTaskFixAction(advanced),
			})
			continue
		}
		// Heartbeat-only zombie detection: running task whose dispatch state
		// is no longer the issue's current state. The agent has demonstrably
		// finished (label already flipped) even though the DB row never
		// transitioned off 'running'.
		if advanced {
			findings = append(findings, Finding{
				Kind:      KindOrphanedTask,
				Repo:      task.Repo,
				IssueNum:  task.IssueNum,
				AgentName: task.AgentName,
				Severity:  SeverityError,
				Diagnosis: fmt.Sprintf("task %s (%s) is running for state %q but issue is now in %q — agent already done",
					task.ID, task.AgentName, task.State, cur),
				SuggestedFix: "mark task completed so slot is released; restart worker if heartbeat is stuck",
			})
		}
	}

	hazards, err := st.ListIssuePipelineHazards(repo)
	if err != nil {
		return nil, fmt.Errorf("diagnose: list pipeline hazards: %w", err)
	}
	for _, h := range hazards {
		findings = append(findings, Finding{
			Kind:         KindPipelineHazard,
			Repo:         h.Repo,
			IssueNum:     h.IssueNum,
			Severity:     SeverityWarn,
			Diagnosis:    pipelineHazardDiagnosis(h.Kind),
			SuggestedFix: pipelineHazardSuggestedFix(h.Kind),
		})
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

func loadDiagnoseConfig(configDir string) (diagnoseConfig, error) {
	if strings.TrimSpace(configDir) == "" {
		resolved, err := findConfigDir()
		if err != nil {
			return diagnoseConfig{}, err
		}
		configDir = resolved
	}
	if configDir == "" {
		return diagnoseConfig{
			AgentTimeouts: map[string]time.Duration{},
			IdleThreshold: defaultIdleThreshold,
		}, nil
	}
	cfg, _, err := config.LoadConfig(configDir)
	if err != nil {
		if os.IsNotExist(err) {
			return diagnoseConfig{
				AgentTimeouts: map[string]time.Duration{},
				IdleThreshold: defaultIdleThreshold,
			}, nil
		}
		return diagnoseConfig{}, fmt.Errorf("diagnose: load config: %w", err)
	}
	timeouts := make(map[string]time.Duration, len(cfg.Agents))
	for name, agent := range cfg.Agents {
		if agent.Timeout > 0 {
			timeouts[name] = agent.Timeout
		}
	}
	idleThreshold := cfg.Worker.StaleInference.IdleThreshold
	if idleThreshold <= 0 {
		idleThreshold = defaultIdleThreshold
	}
	return diagnoseConfig{
		AgentTimeouts: timeouts,
		IdleThreshold: idleThreshold,
	}, nil
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

func taskSessionSignal(st *store.Store, task store.TaskRecord, now time.Time, idleThreshold time.Duration) (string, time.Duration, bool, error) {
	record, err := latestSessionForTask(st, task)
	if err != nil || record == nil {
		return "", 0, false, err
	}
	artifactPath := sessionArtifactPath(*record)
	if artifactPath == "" {
		return sessionWorktreeRoot(record.Dir), 0, false, nil
	}
	info, err := statPath(artifactPath)
	if err != nil {
		if os.IsNotExist(err) {
			return sessionWorktreeRoot(record.Dir), 0, false, nil
		}
		return "", 0, false, fmt.Errorf("diagnose: stat session artifact for task %s: %w", task.ID, err)
	}
	idleFor := now.Sub(info.ModTime().UTC())
	return sessionWorktreeRoot(record.Dir), idleFor, idleFor > 2*idleThreshold, nil
}

func latestSessionForTask(st *store.Store, task store.TaskRecord) (*store.SessionRecord, error) {
	records, err := st.ListSessions(store.SessionFilter{
		Repo:      task.Repo,
		IssueNum:  task.IssueNum,
		AgentName: task.AgentName,
	})
	if err != nil {
		return nil, fmt.Errorf("diagnose: list sessions for task %s: %w", task.ID, err)
	}
	for _, record := range records {
		if record.TaskID == task.ID {
			rec := record
			return &rec, nil
		}
	}
	return nil, nil
}

func sessionArtifactPath(record store.SessionRecord) string {
	candidates := []string{
		filepath.Join(record.Dir, "events-v1.jsonl"),
		filepath.Join(record.Dir, "codex-exec.jsonl"),
		record.RawPath,
	}
	var fallback string
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		if fallback == "" {
			fallback = candidate
		}
		if _, err := statPath(candidate); err == nil {
			return candidate
		}
	}
	return fallback
}

func sessionWorktreeRoot(sessionDir string) string {
	dir := filepath.Clean(sessionDir)
	for dir != "" && dir != string(filepath.Separator) && dir != "." {
		if filepath.Base(dir) == ".workbuddy" {
			return filepath.Dir(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func taskMissingChildProcess(task store.TaskRecord, worktreeRoot string, now time.Time, processes []taskProcess) bool {
	if strings.TrimSpace(worktreeRoot) == "" {
		return false
	}
	startedAt := task.CreatedAt
	if !task.AckedAt.IsZero() {
		startedAt = task.AckedAt
	}
	if startedAt.IsZero() || now.Sub(startedAt) < noChildGracePeriod {
		return false
	}
	for _, proc := range processes {
		if !taskProcessMatchesRuntime(task.Runtime, proc.Base) {
			continue
		}
		if isWithinRoot(worktreeRoot, proc.CWD) {
			return false
		}
	}
	return true
}

func taskProcessMatchesRuntime(runtimeName, procBase string) bool {
	switch strings.TrimSpace(runtimeName) {
	case "claude-code", "claude":
		return procBase == "claude"
	case "codex":
		return procBase == "codex"
	default:
		return procBase == "claude" || procBase == "codex"
	}
}

func taskAdvancedPastIssueState(task store.TaskRecord, issueCurrentState map[string]string) (bool, string) {
	if strings.TrimSpace(task.State) == "" {
		return false, ""
	}
	cur, ok := issueCurrentState[issueKey(task.Repo, task.IssueNum)]
	if !ok || cur == "" || cur == task.State {
		return false, cur
	}
	return true, cur
}

func orphanedTaskFixAction(advanced bool) string {
	if advanced {
		return "mark_completed"
	}
	return "mark_failed"
}

func orphanedTaskSuggestedFix(advanced bool) string {
	if advanced {
		return "mark task completed so slot is released; worker heartbeat should stop once it observes terminal status"
	}
	return "mark task failed so coordinator can redispatch cleanly; inspect worker/session artifacts for the zombie source"
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

func pipelineHazardDiagnosis(kind string) string {
	switch kind {
	case store.HazardKindNoWorkflowMatch:
		return "issue carries a status:* label but no workflow trigger label matched, so the state machine cannot enter it"
	case store.HazardKindAwaitingStatusLabel:
		return "issue declares depends_on but has no status:* label, so the state machine cannot enter it and the dependency gate cannot release downstream work"
	default:
		return fmt.Sprintf("pipeline hazard: %s", kind)
	}
}

func pipelineHazardSuggestedFix(kind string) string {
	switch kind {
	case store.HazardKindNoWorkflowMatch:
		return "add the workflow trigger label, e.g. `workbuddy`"
	case store.HazardKindAwaitingStatusLabel:
		return "add `status:blocked` so the gate can evaluate, or `status:developing` if the deps are already satisfied"
	default:
		return "inspect issue labels and body; clear the hazard once the configuration is complete"
	}
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

func isWithinRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
