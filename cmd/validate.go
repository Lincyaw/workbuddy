package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	intvalidate "github.com/Lincyaw/workbuddy/internal/validate"
	"github.com/spf13/cobra"
)

type validateOpts struct {
	configDir string
	format    string
}

type validateResult struct {
	Valid       bool                     `json:"valid"`
	Diagnostics []validateJSONDiagnostic `json:"diagnostics"`
}

type validateJSONDiagnostic struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate workbuddy config files and workflow references",
	Long: `Parse .github/workbuddy/config.yaml, agent markdown files, and workflow
definitions, then check that every referenced agent/role/state resolves.
Reports any undefined references or schema errors. Run this before 'serve'
or 'repo register' to catch config mistakes early.`,
	Example: `  # Validate the repo in the current directory
  workbuddy validate

  # Validate a specific config directory
  workbuddy validate --config-dir /path/to/repo/.github/workbuddy`,
	RunE: runValidateCmd,
}

func init() {
	validateCmd.Flags().String("config-dir", ".github/workbuddy", "Configuration directory to validate")
	addOutputFormatFlag(validateCmd)
	rootCmd.AddCommand(validateCmd)
}

func runValidateCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseValidateFlags(cmd)
	if err != nil {
		return err
	}
	return runValidateWithOpts(cmd.Context(), opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func parseValidateFlags(cmd *cobra.Command) (*validateOpts, error) {
	configDir, _ := cmd.Flags().GetString("config-dir")
	format, err := resolveOutputFormat(cmd, "validate")
	if err != nil {
		return nil, err
	}
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return nil, fmt.Errorf("validate: --config-dir is required")
	}
	return &validateOpts{configDir: configDir, format: format}, nil
}

func runValidateWithOpts(_ context.Context, opts *validateOpts, stdout, stderr io.Writer) error {
	diags, err := intvalidate.ValidateDir(opts.configDir)
	if err != nil {
		return err
	}
	if isJSONOutput(opts.format) {
		result := validateResult{
			Valid:       len(diags) == 0,
			Diagnostics: make([]validateJSONDiagnostic, 0, len(diags)),
		}
		for _, diag := range diags {
			result.Diagnostics = append(result.Diagnostics, validateJSONDiagnostic{
				Path:    diag.Path,
				Line:    diag.Line,
				Message: diag.Message,
			})
		}
		if err := writeJSON(stdout, result); err != nil {
			return err
		}
		if len(diags) > 0 {
			return &cliExitError{code: 1}
		}
		return nil
	}
	if len(diags) == 0 {
		return nil
	}
	for _, diag := range diags {
		_, _ = fmt.Fprintln(stderr, diag.String())
	}
	return &cliExitError{code: exitCodeFailure}
}
