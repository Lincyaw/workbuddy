package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/spf13/cobra"
)

const statusHTTPTimeout = 10 * time.Second

type statusOpts struct {
	repo    string
	stuck   bool
	jsonOut bool
	baseURL string
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

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Summarize issue status from the local audit server",
	RunE:  runStatusCmd,
}

func init() {
	statusCmd.Flags().String("repo", "", "GitHub repository in OWNER/NAME form")
	statusCmd.Flags().Bool("stuck", false, "Only show issues stuck in an intermediate state for more than 1 hour")
	statusCmd.Flags().Bool("json", false, "Emit machine-readable JSON")
	rootCmd.AddCommand(statusCmd)
}

func runStatusCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseStatusFlags(cmd)
	if err != nil {
		return err
	}
	client := &statusClient{
		baseURL: opts.baseURL,
		http:    &http.Client{Timeout: statusHTTPTimeout},
	}
	return runStatusWithOpts(cmd.Context(), opts, client, os.Stdout)
}

func parseStatusFlags(cmd *cobra.Command) (*statusOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	stuck, _ := cmd.Flags().GetBool("stuck")
	jsonOut, _ := cmd.Flags().GetBool("json")

	repo = strings.TrimSpace(repo)
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
		repo:    repo,
		stuck:   stuck,
		jsonOut: jsonOut,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
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

func (c *statusClient) listIssueNums(ctx context.Context, repo string) ([]int, error) {
	u, err := url.Parse(c.baseURL + "/events")
	if err != nil {
		return nil, fmt.Errorf("status: parse events url: %w", err)
	}
	q := u.Query()
	q.Set("repo", repo)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("status: build events request: %w", err)
	}
	var resp audit.EventsResponse
	if err := c.doJSON(req, &resp); err != nil {
		return nil, err
	}

	seen := make(map[int]struct{})
	for _, ev := range resp.Events {
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
