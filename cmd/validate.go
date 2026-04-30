package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/store"
	intvalidate "github.com/Lincyaw/workbuddy/internal/validate"
	"github.com/spf13/cobra"
)

type validateOpts struct {
	configDir      string
	format         string
	strict         bool
	noRuntimeCheck bool
}

type validateResult struct {
	Valid       bool                     `json:"valid"`
	Diagnostics []validateJSONDiagnostic `json:"diagnostics"`
}

type validateJSONDiagnostic struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message"`
}

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate workbuddy config files and workflow references",
	Long: `Parse .github/workbuddy/config.yaml, agent markdown files, and workflow
definitions, then check that every referenced agent/role/state resolves.
Reports any undefined references or schema errors. Run this before 'serve'
or 'repo register' to catch config mistakes early.

Diagnostics carry a severity (error|warning|info) and a stable code
(e.g. WB-X003). Only error-severity diagnostics cause a non-zero exit
unless --strict is passed.`,
	Example: `  # Validate the repo in the current directory
  workbuddy validate

  # Validate a specific config directory
  workbuddy validate --config-dir /path/to/repo/.github/workbuddy

  # Treat warnings as errors (CI mode)
  workbuddy validate --strict

  # Skip runtime binary lookup (CI/sandbox without codex/claude installed)
  workbuddy validate --no-runtime-check`,
	RunE: runValidateCmd,
}

func init() {
	validateCmd.Flags().String("config-dir", ".github/workbuddy", "Configuration directory to validate")
	validateCmd.Flags().Bool("strict", false, "Treat warnings as errors (non-zero exit)")
	validateCmd.Flags().Bool("no-runtime-check", false, "Suppress WB-S003 runtime-binary-on-PATH warnings")
	addOutputFormatFlag(validateCmd)
	rootCmd.AddCommand(validateCmd)
}

func runValidateCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseValidateFlags(cmd)
	if err != nil {
		return err
	}
	return runValidateWithOpts(cmd.Context(), opts, cmdStdout(cmd), cmdStderr(cmd))
}

func parseValidateFlags(cmd *cobra.Command) (*validateOpts, error) {
	configDir, _ := cmd.Flags().GetString("config-dir")
	format, err := resolveOutputFormat(cmd, "validate")
	if err != nil {
		return nil, err
	}
	strict, _ := cmd.Flags().GetBool("strict")
	noRuntimeCheck, _ := cmd.Flags().GetBool("no-runtime-check")
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return nil, fmt.Errorf("validate: --config-dir is required")
	}
	return &validateOpts{
		configDir:      configDir,
		format:         format,
		strict:         strict,
		noRuntimeCheck: noRuntimeCheck,
	}, nil
}

func runValidateWithOpts(_ context.Context, opts *validateOpts, stdout, stderr io.Writer) error {
	validateOptions := intvalidate.Options{
		SkipRuntimeBinaryCheck: opts.noRuntimeCheck,
	}
	if runtimes, ok, err := loadAdvertisedWorkerRuntimes(opts.configDir); err != nil {
		return err
	} else if ok {
		validateOptions.WorkerRuntimes = runtimes
		validateOptions.CheckWorkerRuntimes = true
	}
	diags, err := intvalidate.ValidateDirWithOptions(opts.configDir, validateOptions)
	if err != nil {
		return err
	}

	hasError, hasWarning := classifyDiagnostics(diags)
	failing := hasError || (opts.strict && hasWarning)

	if isJSONOutput(opts.format) {
		result := validateResult{
			Valid:       !failing,
			Diagnostics: make([]validateJSONDiagnostic, 0, len(diags)),
		}
		for _, diag := range diags {
			result.Diagnostics = append(result.Diagnostics, validateJSONDiagnostic{
				Path:     diag.Path,
				Line:     diag.Line,
				Severity: string(diag.EffectiveSeverity()),
				Code:     diag.Code,
				Message:  diag.Message,
			})
		}
		if err := writeJSON(stdout, result); err != nil {
			return err
		}
		if failing {
			return &cliExitError{code: 1}
		}
		return nil
	}
	for _, diag := range diags {
		_, _ = fmt.Fprintln(stderr, diag.String())
	}
	if failing {
		return &cliExitError{code: exitCodeFailure}
	}
	return nil
}

// classifyDiagnostics returns whether the slice contains error- and
// warning-severity entries. Any unset Severity defaults to error.
func classifyDiagnostics(diags []intvalidate.Diagnostic) (hasError, hasWarning bool) {
	for _, d := range diags {
		switch d.EffectiveSeverity() {
		case intvalidate.SeverityError:
			hasError = true
		case intvalidate.SeverityWarning:
			hasWarning = true
		}
	}
	return
}

func loadAdvertisedWorkerRuntimes(configDir string) ([]string, bool, error) {
	dbPath, ok, err := localWorkerDBPath(configDir)
	if err != nil || !ok {
		return nil, ok, err
	}
	st, err := store.NewStore(dbPath)
	if err != nil {
		return nil, false, fmt.Errorf("validate: open worker registry %q: %w", dbPath, err)
	}
	defer func() { _ = st.Close() }()

	workers, err := st.QueryWorkers("")
	if err != nil {
		return nil, false, fmt.Errorf("validate: query workers from %q: %w", dbPath, err)
	}
	seen := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		runtime := strings.TrimSpace(worker.Runtime)
		if runtime == "" {
			continue
		}
		seen[runtime] = struct{}{}
	}
	runtimes := make([]string, 0, len(seen))
	for runtime := range seen {
		runtimes = append(runtimes, runtime)
	}
	sort.Strings(runtimes)
	return runtimes, true, nil
}

func localWorkerDBPath(configDir string) (string, bool, error) {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return "", false, nil
	}
	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		return "", false, fmt.Errorf("validate: abs config dir: %w", err)
	}
	repoRoot := filepath.Dir(absConfigDir)
	if filepath.Base(absConfigDir) == "workbuddy" && filepath.Base(repoRoot) == ".github" {
		repoRoot = filepath.Dir(repoRoot)
	}
	dbPath := filepath.Join(repoRoot, ".workbuddy", "workbuddy.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("validate: stat worker registry %q: %w", dbPath, err)
	}
	return dbPath, true, nil
}
