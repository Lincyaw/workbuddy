package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

type restartIssueOpts struct {
	repo        string
	issue       int
	dbPath      string
	source      string
	jsonOut     bool
	force       bool
	dryRun      bool
	interactive bool
	stdin       io.Reader
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

var issueRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Force an issue back through the next poll cycle",
	Long: `Clear the local recovery state for one issue so the next poll cycle
treats it as fresh. This removes the issue's poller cache row, resets any
cached dependency verdict, and clears a lingering issue-claim lease when one
exists. Use this when an issue is stuck in status:developing/status:reviewing
and simple label toggles are ignored because the poller cache already matches
GitHub.`,
	Example: `  workbuddy issue restart --repo owner/name --issue 173
  workbuddy issue restart --repo owner/name --issue 173 --format json`,
	RunE: runRestartIssueCmd,
}

var adminRestartIssueCmd = &cobra.Command{
	Use:   "restart-issue",
	Short: "Force an issue back through the next poll cycle",
	Long: `Deprecated alias for "workbuddy issue restart". Clear the local recovery
state for one issue so the next poll cycle treats it as fresh.`,
	Example: `  workbuddy admin restart-issue --repo owner/name --issue 173
  workbuddy admin restart-issue --repo owner/name --issue 173 --format json`,
	RunE: runAdminRestartIssueCmd,
}

func init() {
	bindRestartIssueFlags(issueRestartCmd)
	bindRestartIssueFlags(adminRestartIssueCmd)
	issueCmd.AddCommand(issueRestartCmd)
	adminCmd.AddCommand(adminRestartIssueCmd)
}

func runRestartIssueCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseRestartIssueFlags(cmd)
	if err != nil {
		return err
	}
	return runRestartIssueWithOpts(cmd.Context(), opts, cmd.OutOrStdout())
}

func runAdminRestartIssueCmd(cmd *cobra.Command, args []string) error {
	writeDeprecationWarning(cmd.ErrOrStderr(), "`workbuddy admin restart-issue`", "`workbuddy issue restart`")
	return runRestartIssueCmd(cmd, args)
}

func bindRestartIssueFlags(cmd *cobra.Command) {
	cmd.Flags().String("repo", "", "GitHub repository in OWNER/NAME form")
	cmd.Flags().Int("issue", 0, "Issue number to restart")
	cmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path")
	addOutputFormatFlag(cmd)
	addDeprecatedJSONAliasFlag(cmd)
	cmd.Flags().Bool("force", false, "Skip confirmation prompts for destructive actions")
	cmd.Flags().Bool("dry-run", false, "Print the actions that would be taken without executing them")
}

func parseRestartIssueFlags(cmd *cobra.Command) (*restartIssueOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	issue, _ := cmd.Flags().GetInt("issue")
	dbPath, _ := cmd.Flags().GetString("db-path")
	format, err := resolveOutputFormat(cmd, "restart-issue")
	if err != nil {
		return nil, err
	}
	force, _ := cmd.Flags().GetBool("force")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

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
		repo:        repo,
		issue:       issue,
		dbPath:      dbPath,
		source:      "cli:admin:restart-issue",
		jsonOut:     isJSONOutput(format),
		force:       force,
		dryRun:      dryRun,
		stdin:       cmd.InOrStdin(),
		interactive: commandIsInteractiveTerminal(),
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

	preview, err := inspectRestartIssueStore(st, opts.repo, opts.issue)
	if err != nil {
		return err
	}
	if opts.dryRun {
		return writeRestartIssueResult(stdout, preview, true, opts.jsonOut)
	}
	ok, err := confirmDestructiveAction(
		"restart-issue",
		opts.stdin,
		stdout,
		opts.interactive,
		opts.force,
		opts.dryRun,
		fmt.Sprintf("Restart %s#%d?", preview.Repo, preview.IssueNum),
		[]string{
			fmt.Sprintf("clear issue cache: %t", preview.CacheCleared),
			fmt.Sprintf("clear dependency state: %t", preview.DependencyStateCleared),
			fmt.Sprintf("clear issue claim: %t", preview.ClaimCleared),
			"log issue_restarted event",
		},
	)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	result, err := runRestartIssueStore(st, opts.repo, opts.issue, opts.source)
	if err != nil {
		return err
	}
	return writeRestartIssueResult(stdout, result, false, opts.jsonOut)
}

func writeRestartIssueResult(stdout io.Writer, result restartIssueResult, dryRun, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if dryRun {
			return enc.Encode(struct {
				restartIssueResult
				DryRun bool `json:"dry_run"`
			}{
				restartIssueResult: result,
				DryRun:             true,
			})
		}
		return enc.Encode(result)
	}
	prefix := ""
	if dryRun {
		prefix = "dry-run: "
	}
	_, _ = fmt.Fprintf(stdout, "%s%s#%d: cache=%t dependency_state=%t claim=%t", prefix, result.Repo, result.IssueNum, result.CacheCleared, result.DependencyStateCleared, result.ClaimCleared)
	if result.ClaimOwner != "" {
		_, _ = fmt.Fprintf(stdout, " held_by=%s", result.ClaimOwner)
	}
	if dryRun {
		_, _ = fmt.Fprintf(stdout, " event=true")
	}
	_, _ = fmt.Fprintln(stdout)
	return nil
}

func inspectRestartIssueStore(st *store.Store, repo string, issueNum int) (restartIssueResult, error) {
	result := restartIssueResult{Repo: repo, IssueNum: issueNum}

	cached, err := st.QueryIssueCache(repo, issueNum)
	if err != nil {
		return result, fmt.Errorf("restart-issue: query issue cache #%d: %w", issueNum, err)
	}
	result.CacheCleared = cached != nil

	depState, err := st.QueryIssueDependencyState(repo, issueNum)
	if err != nil {
		return result, fmt.Errorf("restart-issue: query dependency state #%d: %w", issueNum, err)
	}
	result.DependencyStateCleared = depState != nil

	claim, err := st.QueryIssueClaim(repo, issueNum)
	if err != nil {
		return result, fmt.Errorf("restart-issue: query issue claim #%d: %w", issueNum, err)
	}
	if claim != nil {
		result.ClaimCleared = true
		result.ClaimOwner = claim.WorkerID
	}
	return result, nil
}

func runRestartIssueStore(st *store.Store, repo string, issueNum int, source string) (restartIssueResult, error) {
	result, err := inspectRestartIssueStore(st, repo, issueNum)
	if err != nil {
		return result, err
	}

	if result.CacheCleared {
		if err := st.DeleteIssueCache(repo, issueNum); err != nil {
			return result, fmt.Errorf("restart-issue: delete issue cache #%d: %w", issueNum, err)
		}
	}
	if result.DependencyStateCleared {
		if _, err := st.DeleteIssueDependencyState(repo, issueNum); err != nil {
			return result, fmt.Errorf("restart-issue: reset dependency state #%d: %w", issueNum, err)
		}
	}
	if result.ClaimCleared {
		deletedClaim, err := st.DeleteIssueClaim(repo, issueNum)
		if err != nil {
			return result, fmt.Errorf("restart-issue: delete issue claim #%d: %w", issueNum, err)
		}
		result.ClaimCleared = deletedClaim
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
