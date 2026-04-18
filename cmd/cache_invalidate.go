package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
}

type cacheInvalidateResult struct {
	Repo                    string `json:"repo"`
	IssueNum                int    `json:"issue_num"`
	Result                  string `json:"result"`
	DependencyStateCleared  bool   `json:"dependency_state_cleared"`
	EventLogged             bool   `json:"event_logged"`
}

var cacheInvalidateCmd = &cobra.Command{
	Use:   "cache-invalidate",
	Short: "Force the poller to re-process issues on the next cycle",
	Long: `Clear the poller's cached issue snapshot so the next poll treats each
listed issue as new. Use this when an issue's labels changed on GitHub but
workbuddy didn't react (poller de-dup kept it out of the state machine),
or after manually editing labels to kick off a retry.

Omit --issue to invalidate all cached issues for the repo.`,
	Example: `  # Force re-poll of one issue
  workbuddy cache-invalidate --repo owner/name --issue 42

  # Invalidate several issues at once
  workbuddy cache-invalidate --repo owner/name --issue 42,43,44

  # Invalidate every cached issue for a repo
  workbuddy cache-invalidate --repo owner/name`,
	RunE: runCacheInvalidateCmd,
}

func init() {
	cacheInvalidateCmd.Flags().String("repo", "", "GitHub repository in OWNER/NAME form")
	cacheInvalidateCmd.Flags().String("issue", "", "Comma-separated issue numbers")
	cacheInvalidateCmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path")
	cacheInvalidateCmd.Flags().Bool("json", false, "Emit machine-readable JSON")
	rootCmd.AddCommand(cacheInvalidateCmd)
}

func runCacheInvalidateCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseCacheInvalidateFlags(cmd)
	if err != nil {
		return err
	}
	return runCacheInvalidateWithOpts(cmd.Context(), opts, os.Stdout)
}

func parseCacheInvalidateFlags(cmd *cobra.Command) (*cacheInvalidateOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	rawIssues, _ := cmd.Flags().GetString("issue")
	dbPath, _ := cmd.Flags().GetString("db-path")
	jsonOut, _ := cmd.Flags().GetBool("json")

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
		jsonOut: jsonOut,
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

	results, err := runCacheInvalidateStore(st, opts.repo, opts.issues, opts.source)
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
		if verb == "skipped" {
			verb = "not in cache"
		}
		_, _ = fmt.Fprintf(stdout, "%s#%d: %s\n", result.Repo, result.IssueNum, verb)
	}
	_, _ = fmt.Fprintf(stdout, "Events logged: %d\n", len(results))
	return nil
}

func runCacheInvalidateStore(st *store.Store, repo string, issues []int, source string) ([]cacheInvalidateResult, error) {
	results := make([]cacheInvalidateResult, 0, len(issues))
	for _, issueNum := range issues {
		cached, err := st.QueryIssueCache(repo, issueNum)
		if err != nil {
			return nil, fmt.Errorf("cache-invalidate: query issue cache #%d: %w", issueNum, err)
		}
		if cached != nil {
			if err := st.DeleteIssueCache(repo, issueNum); err != nil {
				return nil, fmt.Errorf("cache-invalidate: delete issue cache #%d: %w", issueNum, err)
			}
		}
		deletedDepState, err := st.DeleteIssueDependencyState(repo, issueNum)
		if err != nil {
			return nil, fmt.Errorf("cache-invalidate: reset dependency state #%d: %w", issueNum, err)
		}
		payload := fmt.Sprintf(`{"repo":%q,"issue_num":%d,"source":%q}`, repo, issueNum, source)
		if _, err := st.InsertEvent(store.Event{
			Type:     "cache_invalidated",
			Repo:     repo,
			IssueNum: issueNum,
			Payload:  payload,
		}); err != nil {
			return nil, fmt.Errorf("cache-invalidate: log event #%d: %w", issueNum, err)
		}
		result := cacheInvalidateResult{
			Repo:                   repo,
			IssueNum:               issueNum,
			Result:                 "skipped",
			DependencyStateCleared: deletedDepState,
			EventLogged:            true,
		}
		if cached != nil {
			result.Result = "deleted"
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
