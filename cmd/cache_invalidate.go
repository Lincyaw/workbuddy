package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

type cacheInvalidateOpts struct {
	repo    string
	issues  []int
	dbPath  string
	source  string
	jsonOut bool
	dryRun  bool
}

type cacheInvalidateResult struct {
	Repo                   string `json:"repo"`
	IssueNum               int    `json:"issue_num"`
	Result                 string `json:"result"`
	DependencyStateCleared bool   `json:"dependency_state_cleared"`
	EventLogged            bool   `json:"event_logged"`
}

var cacheInvalidateCmd = &cobra.Command{
	Use:   "invalidate",
	Short: "Force the poller to re-process issues on the next cycle",
	Long: `Clear the poller's cached issue snapshot so the next poll treats each
listed issue as new. Use this when an issue's labels changed on GitHub but
workbuddy didn't react (poller de-dup kept it out of the state machine),
or after manually editing labels to kick off a retry.`,
	Example: `  # Force re-poll of one issue
  workbuddy cache invalidate --repo owner/name --issue 42

  # Invalidate several issues at once
  workbuddy cache invalidate --repo owner/name --issue 42,43,44`,
	RunE: runCacheInvalidateCmd,
}

var cacheInvalidateAliasCmd = &cobra.Command{
	Use:   "cache-invalidate",
	Short: "Force the poller to re-process issues on the next cycle",
	Long: `Clear the poller's cached issue snapshot so the next poll treats each
listed issue as new. Use this when an issue's labels changed on GitHub but
workbuddy didn't react (poller de-dup kept it out of the state machine),
or after manually editing labels to kick off a retry.`,
	Example: `  # Force re-poll of one issue
  workbuddy cache-invalidate --repo owner/name --issue 42

  # Invalidate several issues at once
  workbuddy cache-invalidate --repo owner/name --issue 42,43,44`,
	RunE: runCacheInvalidateAliasCmd,
}

func init() {
	bindCacheInvalidateFlags(cacheInvalidateCmd)
	bindCacheInvalidateFlags(cacheInvalidateAliasCmd)
	cacheCmd.AddCommand(cacheInvalidateCmd)
	rootCmd.AddCommand(cacheInvalidateAliasCmd)
}

func runCacheInvalidateCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseCacheInvalidateFlags(cmd)
	if err != nil {
		return err
	}
	if err := requireWritable(cmd, "cache-invalidate"); err != nil {
		return err
	}
	return runCacheInvalidateWithOpts(cmd.Context(), opts, cmdStdout(cmd))
}

func runCacheInvalidateAliasCmd(cmd *cobra.Command, args []string) error {
	writeDeprecationWarning(cmd.ErrOrStderr(), "`workbuddy cache-invalidate`", "`workbuddy cache invalidate`")
	return runCacheInvalidateCmd(cmd, args)
}

func bindCacheInvalidateFlags(cmd *cobra.Command) {
	cmd.Flags().String("repo", "", "GitHub repository in OWNER/NAME form")
	cmd.Flags().String("issue", "", "Comma-separated issue numbers")
	cmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path")
	addOutputFormatFlag(cmd)
	addDeprecatedJSONAliasFlag(cmd)
	cmd.Flags().Bool("dry-run", false, "Print the actions that would be taken without executing them")
}

func parseCacheInvalidateFlags(cmd *cobra.Command) (*cacheInvalidateOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	rawIssues, _ := cmd.Flags().GetString("issue")
	dbPath, _ := cmd.Flags().GetString("db-path")
	format, err := resolveOutputFormat(cmd, "cache-invalidate")
	if err != nil {
		return nil, err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil, fmt.Errorf("cache-invalidate: --repo is required")
	}
	issues, err := parseIssueList(rawIssues)
	if err != nil {
		return nil, fmt.Errorf("cache-invalidate: %w", err)
	}
	if len(issues) == 0 {
		return nil, fmt.Errorf("cache-invalidate: --issue is required")
	}
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return nil, fmt.Errorf("cache-invalidate: --db-path is required")
	}

	return &cacheInvalidateOpts{
		repo:    repo,
		issues:  issues,
		dbPath:  dbPath,
		source:  "cli:cache-invalidate",
		jsonOut: isJSONOutput(format),
		dryRun:  dryRun,
	}, nil
}

func runCacheInvalidateWithOpts(_ context.Context, opts *cacheInvalidateOpts, stdout io.Writer) error {
	dbPath, err := filepath.Abs(opts.dbPath)
	if err != nil {
		return fmt.Errorf("cache-invalidate: resolve db path: %w", err)
	}
	st, err := store.NewStore(dbPath)
	if err != nil {
		return fmt.Errorf("cache-invalidate: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	results, err := runCacheInvalidateStore(st, opts.repo, opts.issues, opts.source, opts.dryRun)
	if err != nil {
		return err
	}
	if opts.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}

	for _, result := range results {
		verb := result.Result
		switch verb {
		case "skipped":
			verb = "not in cache"
		case "would_skip":
			verb = "not in cache (dry-run)"
		case "would_delete":
			verb = "would delete"
		}
		_, _ = fmt.Fprintf(stdout, "%s#%d: %s\n", result.Repo, result.IssueNum, verb)
	}
	if opts.dryRun {
		_, _ = fmt.Fprintf(stdout, "dry-run: would log %d cache_invalidated event(s)\n", len(results))
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "Events logged: %d\n", len(results))
	return nil
}

func runCacheInvalidateStore(st *store.Store, repo string, issues []int, source string, dryRun bool) ([]cacheInvalidateResult, error) {
	results := make([]cacheInvalidateResult, 0, len(issues))
	for _, issueNum := range issues {
		cached, err := st.QueryIssueCache(repo, issueNum)
		if err != nil {
			return nil, fmt.Errorf("cache-invalidate: query issue cache #%d: %w", issueNum, err)
		}
		if cached != nil && !dryRun {
			if err := st.DeleteIssueCache(repo, issueNum); err != nil {
				return nil, fmt.Errorf("cache-invalidate: delete issue cache #%d: %w", issueNum, err)
			}
		}
		var deletedDepState bool
		if dryRun {
			depState, err := st.QueryIssueDependencyState(repo, issueNum)
			if err != nil {
				return nil, fmt.Errorf("cache-invalidate: query dependency state #%d: %w", issueNum, err)
			}
			deletedDepState = depState != nil
		} else {
			deletedDepState, err = st.DeleteIssueDependencyState(repo, issueNum)
		}
		if err != nil {
			return nil, fmt.Errorf("cache-invalidate: reset dependency state #%d: %w", issueNum, err)
		}
		if !dryRun {
			payload := fmt.Sprintf(`{"repo":%q,"issue_num":%d,"source":%q}`, repo, issueNum, source)
			if _, err := st.InsertEvent(store.Event{
				Type:     "cache_invalidated",
				Repo:     repo,
				IssueNum: issueNum,
				Payload:  payload,
			}); err != nil {
				return nil, fmt.Errorf("cache-invalidate: log event #%d: %w", issueNum, err)
			}
		}
		result := cacheInvalidateResult{
			Repo:                   repo,
			IssueNum:               issueNum,
			Result:                 "skipped",
			DependencyStateCleared: deletedDepState,
			EventLogged:            !dryRun,
		}
		if cached != nil {
			result.Result = "deleted"
		}
		if dryRun {
			if cached != nil {
				result.Result = "would_delete"
			} else {
				result.Result = "would_skip"
			}
		}
		results = append(results, result)
	}
	return results, nil
}

func parseIssueList(raw string) ([]int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	issues := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		issueNum, err := strconv.Atoi(part)
		if err != nil || issueNum <= 0 {
			return nil, fmt.Errorf("invalid issue list %q", raw)
		}
		if _, ok := seen[issueNum]; ok {
			continue
		}
		seen[issueNum] = struct{}{}
		issues = append(issues, issueNum)
	}
	return issues, nil
}
