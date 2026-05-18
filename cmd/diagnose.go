package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	Long: `Inspect the coordinator's SQLite state and surface actionable findings
such as issues stuck in an intermediate state, consecutive-agent-failure caps
hit (REQ-055), stale inflight claims, and repos missing config.

Each finding includes a severity, a plain-English diagnosis, and a suggested
fix. Pass --fix to apply safe automated remediations (for example cache
invalidation for stuck issues); destructive actions are never auto-applied.

Use --format json when piping into another tool. Exit code is non-zero if any
error-severity findings remain after --fix.

This command is DB-local only: it does not accept --coordinator. For a remote
coordinator, SSH to the coordinator host and pass --db-path for that
deployment's SQLite database.`,
	Example: `  # Scan all repos
  workbuddy diagnose

  # Focus on one repo, emit JSON
  workbuddy diagnose --repo owner/name --format json

  # Apply safe fixes (cache invalidation, etc.)
  workbuddy diagnose --fix`,
	RunE: runDiagnoseCmd,
}

func init() {
	diagnoseCmd.Flags().String("repo", "", "GitHub repository in OWNER/NAME form")
	diagnoseCmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path")
	diagnoseCmd.Flags().Bool("fix", false, "Apply safe fixes such as cache invalidation")
	addOutputFormatFlag(diagnoseCmd)
	rootCmd.AddCommand(diagnoseCmd)
}

func runDiagnoseCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDiagnoseFlags(cmd)
	if err != nil {
		return err
	}
	if opts.fix {
		if err := requireWritable(cmd, "diagnose --fix"); err != nil {
			return err
		}
	}
	return runDiagnoseWithOpts(cmd.Context(), opts, cmdStdout(cmd))
}

func parseDiagnoseFlags(cmd *cobra.Command) (*diagnoseOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	dbPath, _ := cmd.Flags().GetString("db-path")
	fix, _ := cmd.Flags().GetBool("fix")
	format, err := resolveOutputFormat(cmd, "diagnose")
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(dbPath) == "" {
		return nil, fmt.Errorf("diagnose: --db-path is required")
	}
	return &diagnoseOpts{
		repo:    strings.TrimSpace(repo),
		dbPath:  strings.TrimSpace(dbPath),
		fix:     fix,
		jsonOut: isJSONOutput(format),
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

	// Compute `now` once so the structured finding and the text-mode
	// tunnel status line evaluate heartbeat freshness against the same
	// instant — otherwise a worker straddling the 45s threshold could
	// emit a finding while the line below reports "connected".
	nowUTC := opts.now().UTC()
	findings, err := diag.Analyze(st, opts.repo, nowUTC)
	if err != nil {
		return err
	}
	results := make([]diagnoseResult, 0, len(findings))
	if opts.fix {
		for _, finding := range findings {
			result := diagnoseResult{Finding: finding}
			if finding.AutoFixable {
				if err := applyDiagnoseFindingFix(st, finding); err != nil {
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

	switch {
	case opts.jsonOut:
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	case len(results) == 0:
		_, _ = fmt.Fprintln(stdout, "Pipeline healthy: no issues detected")
		renderDiagnoseTunnelStatus(stdout, st, nowUTC)
	default:
		renderDiagnoseTable(stdout, results)
		// When the table already carries a worker_tunnel_down finding the
		// separate tunnel line would just restate it, so skip it. Callers
		// who want the structured form should use --format json.
		if !findingsIncludeTunnelDown(results) {
			renderDiagnoseTunnelStatus(stdout, st, nowUTC)
		}
	}

	if len(results) > 0 {
		return &cliExitError{code: exitCodeFailure}
	}
	return nil
}

func findingsIncludeTunnelDown(results []diagnoseResult) bool {
	for _, r := range results {
		if r.Kind == diag.KindWorkerTunnelDown {
			return true
		}
	}
	return false
}

// renderDiagnoseTunnelStatus prints a one-line summary derived from the
// same heuristic as KindWorkerTunnelDown: an online worker with a fresh
// last_heartbeat counts as "connected", regardless of the legacy
// `workers.tunnel` column. See KindWorkerTunnelDown for why the column
// is no longer consulted (#345 Wave 3).
func renderDiagnoseTunnelStatus(w io.Writer, st store.Store, now time.Time) {
	workers, err := st.QueryWorkers("")
	if err != nil {
		_, _ = fmt.Fprintf(w, "tunnel: disconnected (last_handshake=unknown; status_error=%v)\n", err)
		return
	}
	// `latest` tracks the most-recently-heartbeating worker across ALL
	// statuses so the disconnected fallback below can show a meaningful
	// last_handshake. Note: this widened from "only Tunnel=true workers"
	// in #345 Wave 3 — we no longer gate on the legacy Tunnel column, so
	// the field is reused as "most recent activity from any worker".
	var latest store.WorkerRecord
	for _, worker := range workers {
		if latest.ID == "" || worker.LastHeartbeat.After(latest.LastHeartbeat) {
			latest = worker
		}
		if !diag.IsHealthyTunneledWorker(now, worker) {
			continue
		}
		_, _ = fmt.Fprintf(w, "tunnel: connected (worker=%s last_handshake=%s)\n", worker.ID, formatTunnelHandshake(worker.LastHeartbeat))
		return
	}
	_, _ = fmt.Fprintf(w, "tunnel: disconnected (last_handshake=%s)\n", formatTunnelHandshake(latest.LastHeartbeat))
}

func formatTunnelHandshake(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

func applyDiagnoseFindingFix(st store.Store, finding diag.Finding) error {
	switch finding.FixAction {
	case "", "cache_invalidate":
		_, err := runCacheInvalidateStore(st, finding.Repo, []int{finding.IssueNum}, "cli:diagnose --fix", false)
		return err
	case "mark_completed":
		return st.FinalizeTaskForOperator(finding.TaskID, store.TaskStatusCompleted, 0)
	case "mark_failed":
		return st.FinalizeTaskForOperator(finding.TaskID, store.TaskStatusFailed, 1)
	default:
		return fmt.Errorf("unsupported fix action %q", finding.FixAction)
	}
}

func renderDiagnoseTable(w io.Writer, rows []diagnoseResult) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ISSUE\tSEVERITY\tDIAGNOSIS\tSUGGESTED FIX")
	for _, row := range rows {
		fix := row.SuggestedFix
		if row.FixApplied {
			fix += " (applied)"
		}
		_, _ = fmt.Fprintf(tw, "%s#%d\t%s\t%s\t%s\n",
			row.Repo, row.IssueNum, row.Severity, row.Diagnosis, fix)
	}
	_ = tw.Flush()
}
