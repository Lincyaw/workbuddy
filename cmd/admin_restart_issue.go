package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

type restartIssueOpts struct {
	repo    string
	issue   int
	dbPath  string
	source  string
	jsonOut bool
}

type restartIssueResult struct {
	Repo                   string `json:"repo"`
	IssueNum               int    `json:"issue_num"`
	CacheCleared           bool   `json:"cache_cleared"`
	DependencyStateCleared bool   `json:"dependency_state_cleared"`
	ClaimCleared           bool   `json:"claim_cleared"`
	ClaimOwner             string `json:"claim_owner,omitempty"`
	EventLogged            bool   `json:"event_logged"`
}

var restartIssueCmd = &cobra.Command{
	Use:   "restart-issue",
	Short: "Force an issue back through the next poll cycle",
	Long: `Clear the local recovery state for one issue so the next poll cycle
 treats it as fresh. This removes the issue's poller cache row, resets any
 cached dependency verdict, and clears a lingering issue-claim lease when one
 exists. Use this when an issue is stuck in status:developing/status:reviewing
 and simple label toggles are ignored because the poller cache already matches
 GitHub.`,
	Example: `  workbuddy admin restart-issue --repo owner/name --issue 173
  workbuddy admin restart-issue --repo owner/name --issue 173 --json`,
	RunE: runRestartIssueCmd,
}

func init() {
	restartIssueCmd.Flags().String("repo", "", "GitHub repository in OWNER/NAME form")
	restartIssueCmd.Flags().Int("issue", 0, "Issue number to restart")
	restartIssueCmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path")
	restartIssueCmd.Flags().Bool("json", false, "Emit machine-readable JSON")
	adminCmd.AddCommand(restartIssueCmd)
}

func runRestartIssueCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseRestartIssueFlags(cmd)
	if err != nil {
		return err
	}
	return runRestartIssueWithOpts(cmd.Context(), opts, os.Stdout)
}

func parseRestartIssueFlags(cmd *cobra.Command) (*restartIssueOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	issue, _ := cmd.Flags().GetInt("issue")
	dbPath, _ := cmd.Flags().GetString("db-path")
	jsonOut, _ := cmd.Flags().GetBool("json")

	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil, fmt.Errorf("restart-issue: --repo is required")
	}
	if issue <= 0 {
		return nil, fmt.Errorf("restart-issue: --issue must be > 0")
	}
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return nil, fmt.Errorf("restart-issue: --db-path is required")
	}
	return &restartIssueOpts{
		repo:    repo,
		issue:   issue,
		dbPath:  dbPath,
		source:  "cli:admin:restart-issue",
		jsonOut: jsonOut,
	}, nil
}

func runRestartIssueWithOpts(_ context.Context, opts *restartIssueOpts, stdout io.Writer) error {
	dbPath, err := filepath.Abs(opts.dbPath)
	if err != nil {
		return fmt.Errorf("restart-issue: resolve db path: %w", err)
	}
	st, err := store.NewStore(dbPath)
	if err != nil {
		return fmt.Errorf("restart-issue: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	result, err := runRestartIssueStore(st, opts.repo, opts.issue, opts.source)
	if err != nil {
		return err
	}
	if opts.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	_, _ = fmt.Fprintf(stdout, "%s#%d: cache=%t dependency_state=%t claim=%t", result.Repo, result.IssueNum, result.CacheCleared, result.DependencyStateCleared, result.ClaimCleared)
	if result.ClaimOwner != "" {
		_, _ = fmt.Fprintf(stdout, " held_by=%s", result.ClaimOwner)
	}
	_, _ = fmt.Fprintln(stdout)
	return nil
}

func runRestartIssueStore(st *store.Store, repo string, issueNum int, source string) (restartIssueResult, error) {
	result := restartIssueResult{Repo: repo, IssueNum: issueNum}

	cached, err := st.QueryIssueCache(repo, issueNum)
	if err != nil {
		return result, fmt.Errorf("restart-issue: query issue cache #%d: %w", issueNum, err)
	}
	if cached != nil {
		if err := st.DeleteIssueCache(repo, issueNum); err != nil {
			return result, fmt.Errorf("restart-issue: delete issue cache #%d: %w", issueNum, err)
		}
		result.CacheCleared = true
	}

	deletedDepState, err := st.DeleteIssueDependencyState(repo, issueNum)
	if err != nil {
		return result, fmt.Errorf("restart-issue: reset dependency state #%d: %w", issueNum, err)
	}
	result.DependencyStateCleared = deletedDepState

	claim, err := st.QueryIssueClaim(repo, issueNum)
	if err != nil {
		return result, fmt.Errorf("restart-issue: query issue claim #%d: %w", issueNum, err)
	}
	if claim != nil {
		deletedClaim, err := st.DeleteIssueClaim(repo, issueNum)
		if err != nil {
			return result, fmt.Errorf("restart-issue: delete issue claim #%d: %w", issueNum, err)
		}
		result.ClaimCleared = deletedClaim
		result.ClaimOwner = claim.WorkerID
	}

	payload, err := json.Marshal(map[string]any{
		"repo":                     repo,
		"issue_num":                issueNum,
		"source":                   source,
		"cache_cleared":            result.CacheCleared,
		"dependency_state_cleared": result.DependencyStateCleared,
		"claim_cleared":            result.ClaimCleared,
		"claim_owner":              result.ClaimOwner,
	})
	if err != nil {
		return result, fmt.Errorf("restart-issue: marshal event payload #%d: %w", issueNum, err)
	}
	if _, err := st.InsertEvent(store.Event{
		Type:     eventlog.TypeIssueRestarted,
		Repo:     repo,
		IssueNum: issueNum,
		Payload:  string(payload),
	}); err != nil {
		return result, fmt.Errorf("restart-issue: log event #%d: %w", issueNum, err)
	}
	result.EventLogged = true
	return result, nil
}
