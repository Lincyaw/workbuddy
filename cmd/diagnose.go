package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	diag "github.com/Lincyaw/workbuddy/internal/diagnose"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

type diagnoseOpts struct {
	repo    string
	dbPath  string
	fix     bool
	jsonOut bool
	now     func() time.Time
}

type diagnoseResult struct {
	diag.Finding
	FixApplied bool `json:"fix_applied,omitempty"`
}

var diagnoseCmd = &cobra.Command{
	Use:   "diagnose",
	Short: "Scan the local SQLite store for common pipeline failures",
	RunE:  runDiagnoseCmd,
}

func init() {
	diagnoseCmd.Flags().String("repo", "", "GitHub repository in OWNER/NAME form")
	diagnoseCmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path")
	diagnoseCmd.Flags().Bool("fix", false, "Apply safe fixes such as cache invalidation")
	diagnoseCmd.Flags().Bool("json", false, "Emit machine-readable JSON")
	rootCmd.AddCommand(diagnoseCmd)
}

func runDiagnoseCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDiagnoseFlags(cmd)
	if err != nil {
		return err
	}
	return runDiagnoseWithOpts(cmd.Context(), opts, os.Stdout)
}

func parseDiagnoseFlags(cmd *cobra.Command) (*diagnoseOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	dbPath, _ := cmd.Flags().GetString("db-path")
	fix, _ := cmd.Flags().GetBool("fix")
	jsonOut, _ := cmd.Flags().GetBool("json")

	if strings.TrimSpace(dbPath) == "" {
		return nil, fmt.Errorf("diagnose: --db-path is required")
	}
	return &diagnoseOpts{
		repo:    strings.TrimSpace(repo),
		dbPath:  strings.TrimSpace(dbPath),
		fix:     fix,
		jsonOut: jsonOut,
		now:     time.Now,
	}, nil
}

func runDiagnoseWithOpts(_ context.Context, opts *diagnoseOpts, stdout io.Writer) error {
	dbPath, err := filepath.Abs(opts.dbPath)
	if err != nil {
		return fmt.Errorf("diagnose: resolve db path: %w", err)
	}
	st, err := store.NewStore(dbPath)
	if err != nil {
		return fmt.Errorf("diagnose: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	findings, err := diag.Analyze(st, opts.repo, opts.now().UTC())
	if err != nil {
		return err
	}
	results := make([]diagnoseResult, 0, len(findings))
	if opts.fix {
		for _, finding := range findings {
			result := diagnoseResult{Finding: finding}
			if finding.AutoFixable {
				if _, err := runCacheInvalidateStore(st, finding.Repo, []int{finding.IssueNum}, "cli:diagnose --fix"); err != nil {
					return fmt.Errorf("diagnose: apply fix for %s#%d: %w", finding.Repo, finding.IssueNum, err)
				}
				payload := fmt.Sprintf(`{"repo":%q,"issue_num":%d,"kind":%q}`, finding.Repo, finding.IssueNum, finding.Kind)
				if _, err := st.InsertEvent(store.Event{
					Type:     "auto_fix",
					Repo:     finding.Repo,
					IssueNum: finding.IssueNum,
					Payload:  payload,
				}); err != nil {
					return fmt.Errorf("diagnose: log auto_fix for %s#%d: %w", finding.Repo, finding.IssueNum, err)
				}
				result.FixApplied = true
			}
			results = append(results, result)
		}
	} else {
		for _, finding := range findings {
			results = append(results, diagnoseResult{Finding: finding})
		}
	}

	if opts.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else if len(results) == 0 {
		_, _ = fmt.Fprintln(stdout, "Pipeline healthy: no issues detected")
	} else {
		renderDiagnoseTable(stdout, results)
	}

	if len(results) > 0 {
		return &cliExitError{code: 1}
	}
	return nil
}

func renderDiagnoseTable(w io.Writer, rows []diagnoseResult) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ISSUE\tSEVERITY\tDIAGNOSIS\tSUGGESTED FIX")
	for _, row := range rows {
		fix := row.SuggestedFix
		if row.FixApplied {
			fix = fix + " (applied)"
		}
		_, _ = fmt.Fprintf(tw, "%s#%d\t%s\t%s\t%s\n",
			row.Repo, row.IssueNum, row.Severity, row.Diagnosis, fix)
	}
	_ = tw.Flush()
}
