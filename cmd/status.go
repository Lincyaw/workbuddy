package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
	"github.com/spf13/cobra"
)

const (
	statusHTTPTimeout    = 10 * time.Second
	defaultWatchTimeout  = 30 * time.Minute
	defaultEventsDisplay = 50
)

type statusOpts struct {
	repo       string
	stuck      bool
	tasks      bool
	events     bool
	watch      bool
	jsonOut    bool
	taskStatus string
	eventType  string
	since      string
	issue      int
	timeout    time.Duration
	baseURL    string
	now        func() time.Time
}

type statusClient struct {
	baseURL string
	http    *http.Client
}

type statusIssue struct {
	IssueNum          int        `json:"issue_num"`
	CurrentState      string     `json:"current_state"`
	CycleCount        int        `json:"cycle_count"`
	DependencyVerdict string     `json:"dependency_verdict"`
	LastEventAt       *time.Time `json:"last_event_at,omitempty"`
	Stuck             bool       `json:"stuck"`
}

type statusResponse struct {
	Repo   string        `json:"repo"`
	Issues []statusIssue `json:"issues"`
}

type statusTaskRow struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	IssueNum  int       `json:"issue_num"`
	AgentName string    `json:"agent_name"`
	Status    string    `json:"status"`
	WorkerID  string    `json:"worker_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type statusEventRow struct {
	ID       int64           `json:"id"`
	TS       time.Time       `json:"ts"`
	Type     string          `json:"type"`
	Repo     string          `json:"repo"`
	IssueNum int             `json:"issue_num,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Summarize issue status from the local audit server",
	RunE:  runStatusCmd,
}

func init() {
	statusCmd.Flags().String("repo", "", "GitHub repository in OWNER/NAME form")
	statusCmd.Flags().Bool("stuck", false, "Only show issues stuck in an intermediate state for more than 1 hour")
	statusCmd.Flags().Bool("tasks", false, "Show task queue entries")
	statusCmd.Flags().Bool("events", false, "Show recent structured events")
	statusCmd.Flags().Bool("watch", false, "Block until the next matching task completes")
	statusCmd.Flags().Bool("json", false, "Emit machine-readable JSON")
	statusCmd.Flags().String("status", "", "Task status filter for --tasks")
	statusCmd.Flags().String("type", "", "Event type filter for --events")
	statusCmd.Flags().String("since", "", "Relative time filter for --events, for example 10m or 1h")
	statusCmd.Flags().Int("issue", 0, "Issue number filter for --watch")
	statusCmd.Flags().Duration("timeout", defaultWatchTimeout, "Maximum time to wait for --watch")
	rootCmd.AddCommand(statusCmd)
}

func runStatusCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseStatusFlags(cmd)
	if err != nil {
		return err
	}
	httpTimeout := statusHTTPTimeout
	if opts.watch {
		httpTimeout = opts.timeout + 5*time.Second
	}
	client := &statusClient{
		baseURL: opts.baseURL,
		http:    &http.Client{Timeout: httpTimeout},
	}
	return runStatusWithOpts(cmd.Context(), opts, client, os.Stdout)
}

func parseStatusFlags(cmd *cobra.Command) (*statusOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	stuck, _ := cmd.Flags().GetBool("stuck")
	tasks, _ := cmd.Flags().GetBool("tasks")
	events, _ := cmd.Flags().GetBool("events")
	watch, _ := cmd.Flags().GetBool("watch")
	jsonOut, _ := cmd.Flags().GetBool("json")
	taskStatus, _ := cmd.Flags().GetString("status")
	eventType, _ := cmd.Flags().GetString("type")
	since, _ := cmd.Flags().GetString("since")
	issue, _ := cmd.Flags().GetInt("issue")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	selected := 0
	for _, enabled := range []bool{stuck, tasks, events, watch} {
		if enabled {
			selected++
		}
	}
	if selected > 1 {
		return nil, fmt.Errorf("status: --stuck, --tasks, --events, and --watch are mutually exclusive")
	}
	if !tasks && strings.TrimSpace(taskStatus) != "" {
		return nil, fmt.Errorf("status: --status requires --tasks")
	}
	if !events && strings.TrimSpace(eventType) != "" {
		return nil, fmt.Errorf("status: --type requires --events")
	}
	if !events && strings.TrimSpace(since) != "" {
		return nil, fmt.Errorf("status: --since requires --events")
	}
	if !watch && issue != 0 {
		return nil, fmt.Errorf("status: --issue requires --watch")
	}
	if !watch && cmd.Flags().Changed("timeout") {
		return nil, fmt.Errorf("status: --timeout requires --watch")
	}
	if issue < 0 {
		return nil, fmt.Errorf("status: --issue must be > 0")
	}
	if timeout <= 0 {
		if watch || cmd.Flags().Changed("timeout") {
			return nil, fmt.Errorf("status: --timeout must be > 0")
		}
		timeout = defaultWatchTimeout
	}

	repo = strings.TrimSpace(repo)
	taskStatus = strings.TrimSpace(taskStatus)
	eventType = strings.TrimSpace(eventType)
	since = strings.TrimSpace(since)

	cfg, err := loadStatusConfig(repo)
	if err != nil {
		return nil, err
	}
	if repo == "" {
		repo = cfg.Global.Repo
	}
	if repo == "" {
		return nil, fmt.Errorf("status: repo is required")
	}

	port := cfg.Global.Port
	if port == 0 {
		port = defaultPort
	}

	return &statusOpts{
		repo:       repo,
		stuck:      stuck,
		tasks:      tasks,
		events:     events,
		watch:      watch,
		jsonOut:    jsonOut,
		taskStatus: taskStatus,
		eventType:  eventType,
		since:      since,
		issue:      issue,
		timeout:    timeout,
		baseURL:    fmt.Sprintf("http://127.0.0.1:%d", port),
		now:        time.Now,
	}, nil
}

func loadStatusConfig(explicitRepo string) (*config.FullConfig, error) {
	if strings.TrimSpace(explicitRepo) != "" {
		if _, err := os.Stat(".github/workbuddy"); err != nil {
			if os.IsNotExist(err) {
				return &config.FullConfig{}, nil
			}
			return nil, fmt.Errorf("status: stat config dir: %w", err)
		}
	}
	cfg, _, err := config.LoadConfig(".github/workbuddy")
	if err == nil {
		return cfg, nil
	}
	return nil, fmt.Errorf("status: load config: %w", err)
}

func runStatusWithOpts(ctx context.Context, opts *statusOpts, client *statusClient, stdout io.Writer) error {
	switch {
	case opts.tasks:
		return runStatusTasks(ctx, opts, client, stdout)
	case opts.events:
		return runStatusEvents(ctx, opts, client, stdout)
	case opts.watch:
		return runStatusWatch(ctx, opts, client, stdout)
	default:
		return runStatusSummary(ctx, opts, client, stdout)
	}
}

func runStatusSummary(ctx context.Context, opts *statusOpts, client *statusClient, stdout io.Writer) error {
	issueNums, err := client.listIssueNums(ctx, opts.repo)
	if err != nil {
		return err
	}

	issues := make([]statusIssue, 0, len(issueNums))
	for _, issueNum := range issueNums {
		issue, err := client.issueState(ctx, opts.repo, issueNum)
		if err != nil {
			return err
		}
		if issue.IssueState != "open" {
			continue
		}
		entry := statusIssue{
			IssueNum:          issue.IssueNum,
			CurrentState:      issue.CurrentState,
			CycleCount:        issue.CycleCount,
			DependencyVerdict: issue.DependencyVerdict,
			LastEventAt:       issue.LastEventAt,
			Stuck:             issue.Stuck,
		}
		if opts.stuck && !entry.Stuck {
			continue
		}
		issues = append(issues, entry)
	}

	sort.Slice(issues, func(i, j int) bool { return issues[i].IssueNum < issues[j].IssueNum })
	resp := statusResponse{Repo: opts.repo, Issues: issues}
	if opts.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	renderStatusTable(stdout, resp)
	return nil
}

func runStatusTasks(ctx context.Context, opts *statusOpts, client *statusClient, stdout io.Writer) error {
	tasks, err := client.listTasks(ctx, opts.repo, opts.taskStatus)
	if err != nil {
		return err
	}

	rows := make([]statusTaskRow, 0, len(tasks))
	for _, task := range tasks {
		if task.Status == store.TaskStatusCompleted {
			continue
		}
		rows = append(rows, statusTaskRow{
			ID:        task.ID,
			Repo:      task.Repo,
			IssueNum:  task.IssueNum,
			AgentName: task.AgentName,
			Status:    task.Status,
			WorkerID:  task.WorkerID,
			UpdatedAt: task.UpdatedAt.UTC(),
		})
	}
	if opts.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	renderTaskTable(stdout, rows)
	return nil
}

func runStatusEvents(ctx context.Context, opts *statusOpts, client *statusClient, stdout io.Writer) error {
	var since *time.Time
	if opts.since != "" {
		d, err := time.ParseDuration(opts.since)
		if err != nil {
			return fmt.Errorf("status: parse --since: %w", err)
		}
		ts := opts.now().Add(-d).UTC()
		since = &ts
	}

	events, err := client.listEvents(ctx, opts.repo, opts.eventType, since)
	if err != nil {
		return err
	}
	rows := make([]statusEventRow, 0, len(events))
	for _, ev := range events {
		rows = append(rows, statusEventRow{
			ID:       ev.ID,
			TS:       ev.TS.UTC(),
			Type:     ev.Type,
			Repo:     ev.Repo,
			IssueNum: ev.IssueNum,
			Payload:  ev.Payload,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].TS.Equal(rows[j].TS) {
			return rows[i].ID > rows[j].ID
		}
		return rows[i].TS.After(rows[j].TS)
	})
	if opts.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) > defaultEventsDisplay {
		rows = rows[:defaultEventsDisplay]
	}
	renderEventTable(stdout, rows)
	return nil
}

func runStatusWatch(ctx context.Context, opts *statusOpts, client *statusClient, stdout io.Writer) error {
	watchCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	_, _ = fmt.Fprintln(stdout, "Waiting for task completion...")
	event, err := client.watchTask(watchCtx, opts.repo, opts.issue)
	if err != nil {
		if watchCtx.Err() == context.DeadlineExceeded {
			_, _ = fmt.Fprintln(stdout, "No task completed within timeout")
			return &cliExitError{code: 3}
		}
		return err
	}
	renderWatchTable(stdout, *event)

	switch event.Status {
	case store.TaskStatusCompleted:
		return nil
	case store.TaskStatusFailed:
		return &cliExitError{code: 1}
	case store.TaskStatusTimeout:
		return &cliExitError{code: 2}
	default:
		return &cliExitError{msg: fmt.Sprintf("status: unknown task status %q", event.Status), code: 1}
	}
}

func (c *statusClient) listIssueNums(ctx context.Context, repo string) ([]int, error) {
	events, err := c.listEvents(ctx, repo, "", nil)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]struct{})
	for _, ev := range events {
		if ev.IssueNum > 0 {
			seen[ev.IssueNum] = struct{}{}
		}
	}
	out := make([]int, 0, len(seen))
	for issueNum := range seen {
		out = append(out, issueNum)
	}
	sort.Ints(out)
	return out, nil
}

func (c *statusClient) listTasks(ctx context.Context, repo, status string) ([]store.TaskRecord, error) {
	u, err := url.Parse(c.baseURL + "/tasks")
	if err != nil {
		return nil, fmt.Errorf("status: parse tasks url: %w", err)
	}
	q := u.Query()
	q.Set("repo", repo)
	if status != "" {
		q.Set("status", status)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("status: build tasks request: %w", err)
	}
	var resp []store.TaskRecord
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *statusClient) listEvents(ctx context.Context, repo, eventType string, since *time.Time) ([]audit.EventEnvelope, error) {
	u, err := url.Parse(c.baseURL + "/events")
	if err != nil {
		return nil, fmt.Errorf("status: parse events url: %w", err)
	}
	q := u.Query()
	q.Set("repo", repo)
	if eventType != "" {
		q.Set("type", eventType)
	}
	if since != nil {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("status: build events request: %w", err)
	}
	var resp audit.EventsResponse
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	return resp.Events, nil
}

func (c *statusClient) issueState(ctx context.Context, repo string, issueNum int) (*audit.IssueStateResponse, error) {
	path := fmt.Sprintf("%s/issues/%s/%d/state", c.baseURL, url.PathEscape(repo), issueNum)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("status: build issue state request: %w", err)
	}
	var resp audit.IssueStateResponse
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *statusClient) watchTask(ctx context.Context, repo string, issue int) (*tasknotify.TaskEvent, error) {
	u, err := url.Parse(c.baseURL + "/tasks/watch")
	if err != nil {
		return nil, fmt.Errorf("status: parse tasks/watch url: %w", err)
	}
	q := u.Query()
	q.Set("repo", repo)
	if issue > 0 {
		q.Set("issue", strconv.Itoa(issue))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("status: build watch request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &cliExitError{msg: "Cannot connect to workbuddy server", code: 1}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = "watch request failed"
		}
		if resp.StatusCode >= 500 {
			msg = "Cannot connect to workbuddy server"
		}
		return nil, &cliExitError{msg: msg, code: 1}
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var event tasknotify.TaskEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil, fmt.Errorf("status: decode watch event: %w", err)
		}
		return &event, nil
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("status: read watch stream: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("status: watch stream closed without task event")
}

func (c *statusClient) doJSON(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("status: request %s: %w", req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status: %s returned %d: %s", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("status: decode %s: %w", req.URL.Path, err)
	}
	return nil
}

func renderStatusTable(w io.Writer, resp statusResponse) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "REPO\tISSUE\tSTATE\tCYCLES\tDEPENDENCY\tLAST EVENT\tSTUCK\n")
	if len(resp.Issues) == 0 {
		_, _ = fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t-\t-\n", resp.Repo)
		_ = tw.Flush()
		return
	}
	for _, issue := range resp.Issues {
		lastEvent := "-"
		if issue.LastEventAt != nil {
			lastEvent = issue.LastEventAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(
			tw,
			"%s\t#%d\t%s\t%d\t%s\t%s\t%t\n",
			resp.Repo,
			issue.IssueNum,
			issue.CurrentState,
			issue.CycleCount,
			issue.DependencyVerdict,
			lastEvent,
			issue.Stuck,
		)
	}
	_ = tw.Flush()
}

func renderTaskTable(w io.Writer, rows []statusTaskRow) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "No tasks found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "REPO\tISSUE\tAGENT\tSTATUS\tWORKER\tUPDATED")
	for _, row := range rows {
		worker := row.WorkerID
		if worker == "" {
			worker = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t#%d\t%s\t%s\t%s\t%s\n",
			row.Repo, row.IssueNum, row.AgentName, row.Status, worker, row.UpdatedAt.Format(time.RFC3339))
	}
	_ = tw.Flush()
}

func renderEventTable(w io.Writer, rows []statusEventRow) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "No events found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tTYPE\tISSUE\tPAYLOAD")
	for _, row := range rows {
		issue := "-"
		if row.IssueNum > 0 {
			issue = fmt.Sprintf("#%d", row.IssueNum)
		}
		payload := strings.TrimSpace(string(row.Payload))
		if payload == "" {
			payload = "-"
		}
		payload = truncateString(payload, 80)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			row.TS.Format(time.RFC3339), row.Type, issue, payload)
	}
	_ = tw.Flush()
}

func renderWatchTable(w io.Writer, event tasknotify.TaskEvent) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ISSUE\tAGENT\tSTATUS\tDURATION\tEXIT")
	_, _ = fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%d\n",
		event.IssueNum,
		event.AgentName,
		event.Status,
		(time.Duration(event.DurationMS) * time.Millisecond).Round(time.Second),
		event.ExitCode,
	)
	_ = tw.Flush()
}

func truncateString(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	if limit <= 3 {
		return s[:limit]
	}
	return s[:limit-3] + "..."
}
