package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/operatorwatch"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

type operatorWatchOpts struct {
	inboxDir   string
	dbPath     string
	configDir  string
	claudePath string
	dryRun     bool
}

type operatorWatchScope string

const (
	operatorWatchScopeGlobal    operatorWatchScope = "global"
	operatorWatchScopeRepoLocal operatorWatchScope = "repo-local"
)

type operatorWatchScopedPath struct {
	flag    string
	raw     string
	display string
	scope   operatorWatchScope
}

var operatorWatchCmd = &cobra.Command{
	Use:   "operator-watch",
	Short: "Watch the operator inbox and dispatch Claude to handle incidents",
	Long: `Tail the operator incident inbox (written by the coordinator when
dispatch is capped, workers are lost, or other out-of-band situations
arise) and spawn a Claude CLI session per incident file. Each handler
gets the incident payload and operates against the live workbuddy
deployment to triage or recover.

Use --dry-run to see which incidents would be handled without actually
launching Claude. Use --claude to point at a specific CLI binary.`,
	Example: `  # Global mode: keep inbox, db, and config together under ~/.workbuddy
  workbuddy operator-watch \
    --inbox ~/.workbuddy/operator/inbox \
    --db ~/.workbuddy/workbuddy.db \
    --config-dir ~/.workbuddy/.github/workbuddy

  # Repo-local mode: keep inbox, db, and config rooted in the current repo
  workbuddy operator-watch \
    --inbox .workbuddy/operator/inbox \
    --db .workbuddy/workbuddy.db \
    --config-dir .github/workbuddy \
    --dry-run`,
	RunE: runOperatorWatchCmd,
}

func init() {
	operatorWatchCmd.Flags().String("inbox", operatorwatch.DefaultInboxPath, "Incident inbox directory")
	operatorWatchCmd.Flags().String("db", operatorwatch.DefaultDBPath, "SQLite database path (must match the inbox scope)")
	operatorWatchCmd.Flags().String("config-dir", operatorwatch.DefaultConfigDir, "Configuration directory (must match the inbox scope)")
	operatorWatchCmd.Flags().String("claude", "", "Path to the claude CLI binary (default: search PATH)")
	operatorWatchCmd.Flags().Bool("dry-run", false, "Log the Claude invocation without spawning it")
	rootCmd.AddCommand(operatorWatchCmd)
}

func runOperatorWatchCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseOperatorWatchFlags(cmd)
	if err != nil {
		return err
	}
	if err := requireWritable(cmd, "operator-watch"); err != nil {
		return err
	}

	st, err := store.NewStore(opts.dbPath)
	if err != nil {
		return fmt.Errorf("operator-watch: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	return operatorwatch.Run(cmd.Context(), operatorwatch.Options{
		InboxDir:   opts.inboxDir,
		ClaudePath: opts.claudePath,
		ConfigDir:  opts.configDir,
		Timeout:    operatorwatch.DefaultTimeout,
		DryRun:     opts.dryRun,
		Stdout:     cmdStdout(cmd),
		Stderr:     cmdStderr(cmd),
		Logger:     eventlog.NewEventLoggerWithWriter(st, cmdStderr(cmd)),
	})
}

func parseOperatorWatchFlags(cmd *cobra.Command) (*operatorWatchOpts, error) {
	inboxDir, _ := cmd.Flags().GetString("inbox")
	dbPath, _ := cmd.Flags().GetString("db")
	configDir, _ := cmd.Flags().GetString("config-dir")
	claudePath, _ := cmd.Flags().GetString("claude")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	inboxDir = strings.TrimSpace(inboxDir)
	if inboxDir == "" {
		return nil, fmt.Errorf("operator-watch: --inbox is required")
	}
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return nil, fmt.Errorf("operator-watch: --db is required")
	}
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return nil, fmt.Errorf("operator-watch: --config-dir is required")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("operator-watch: resolve cwd: %w", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("operator-watch: resolve home: %w", err)
	}

	inboxScope, err := classifyOperatorWatchPath("--inbox", inboxDir, cwd, homeDir)
	if err != nil {
		return nil, err
	}
	dbScope, err := classifyOperatorWatchPath("--db", dbPath, cwd, homeDir)
	if err != nil {
		return nil, err
	}
	configScope, err := classifyOperatorWatchPath("--config-dir", configDir, cwd, homeDir)
	if err != nil {
		return nil, err
	}

	if err := validateOperatorWatchScopes(cwd, inboxScope, dbScope, configScope); err != nil {
		return nil, err
	}

	return &operatorWatchOpts{
		inboxDir:   inboxDir,
		dbPath:     dbPath,
		configDir:  configDir,
		claudePath: strings.TrimSpace(claudePath),
		dryRun:     dryRun,
	}, nil
}

func classifyOperatorWatchPath(flagName, rawPath, cwd, homeDir string) (operatorWatchScopedPath, error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return operatorWatchScopedPath{}, fmt.Errorf("operator-watch: %s is required", flagName)
	}

	globalRoot := filepath.Clean(filepath.Join(homeDir, ".workbuddy"))
	expandedPath, err := expandOperatorWatchUserPath(trimmed, homeDir)
	if err != nil {
		return operatorWatchScopedPath{}, fmt.Errorf("operator-watch: expand %s: %w", flagName, err)
	}

	if filepath.IsAbs(expandedPath) {
		display := filepath.Clean(expandedPath)
		switch {
		case pathWithinScope(display, globalRoot):
			return operatorWatchScopedPath{flag: flagName, raw: trimmed, display: display, scope: operatorWatchScopeGlobal}, nil
		case pathWithinScope(display, cwd):
			return operatorWatchScopedPath{flag: flagName, raw: trimmed, display: display, scope: operatorWatchScopeRepoLocal}, nil
		default:
			return operatorWatchScopedPath{}, fmt.Errorf(
				"operator-watch: %s path %q resolves to %q, outside supported scopes (global root %q, repo root %q)",
				flagName,
				trimmed,
				display,
				globalRoot,
				cwd,
			)
		}
	}

	resolvedPath := filepath.Clean(filepath.Join(cwd, expandedPath))
	if !pathWithinScope(resolvedPath, cwd) {
		return operatorWatchScopedPath{}, fmt.Errorf(
			"operator-watch: %s path %q resolves to %q, outside repo-local root %q",
			flagName,
			trimmed,
			resolvedPath,
			cwd,
		)
	}

	return operatorWatchScopedPath{
		flag:    flagName,
		raw:     trimmed,
		display: trimmed,
		scope:   operatorWatchScopeRepoLocal,
	}, nil
}

func validateOperatorWatchScopes(cwd string, paths ...operatorWatchScopedPath) error {
	if len(paths) == 0 {
		return nil
	}

	expected := paths[0].scope
	for _, scopedPath := range paths[1:] {
		if scopedPath.scope == expected {
			continue
		}

		var details []string
		for _, path := range paths {
			details = append(details, fmt.Sprintf("%s=%q (%s)", path.flag, path.raw, path.scope))
		}
		return fmt.Errorf(
			"operator-watch: scope mismatch: %s; keep --inbox, --db, and --config-dir all global under %q or all repo-local under %q",
			strings.Join(details, ", "),
			filepath.Join("~", ".workbuddy"),
			cwd,
		)
	}
	return nil
}

func expandOperatorWatchUserPath(path, homeDir string) (string, error) {
	if path == "~" {
		return homeDir, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(homeDir, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func pathWithinScope(path, root string) bool {
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
