package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/validatedocs"
	"github.com/spf13/cobra"
)

type validateDocsOpts struct {
	repoRoot string
	format   string
	strict   bool
}

type validateDocsResult struct {
	Valid       bool                     `json:"valid"`
	Diagnostics []validateJSONDiagnostic `json:"diagnostics"`
}

var validateDocsCmd = &cobra.Command{
	Use:   "validate-docs",
	Short: "Validate repo-local docs, skills, and generated plugin drift",
	Long: `Run static consistency checks across documentation-shaped surfaces:
project-index path existence, SKILL.md hygiene, duplicated skill catalogs,
cmd/initdata vs .github agent frontmatter parity, and generated Codex plugin
sync drift.

Diagnostics carry the same severity/code contract as workbuddy validate.
Only error diagnostics fail by default; pass --strict to promote warnings to
non-zero exit for CI.`,
	Example: `  # Validate the current repository's docs/skills/plugin surfaces
  workbuddy validate-docs

  # Treat warnings as failures (CI mode)
  workbuddy validate-docs --strict

  # Validate a different checkout
  workbuddy validate-docs --repo-root /path/to/repo`,
	RunE: runValidateDocsCmd,
}

func init() {
	validateDocsCmd.Flags().String("repo-root", ".", "Repository root to validate")
	validateDocsCmd.Flags().Bool("strict", false, "Treat warnings as errors (non-zero exit)")
	addOutputFormatFlag(validateDocsCmd)
	rootCmd.AddCommand(validateDocsCmd)
}

func runValidateDocsCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseValidateDocsFlags(cmd)
	if err != nil {
		return err
	}
	return runValidateDocsWithOpts(cmd.Context(), opts, cmdStdout(cmd), cmdStderr(cmd))
}

func parseValidateDocsFlags(cmd *cobra.Command) (*validateDocsOpts, error) {
	repoRoot, _ := cmd.Flags().GetString("repo-root")
	format, err := resolveOutputFormat(cmd, "validate-docs")
	if err != nil {
		return nil, err
	}
	strict, _ := cmd.Flags().GetBool("strict")
	repoRoot = strings.TrimSpace(repoRoot)
	if repoRoot == "" {
		return nil, fmt.Errorf("validate-docs: --repo-root is required")
	}
	return &validateDocsOpts{repoRoot: repoRoot, format: format, strict: strict}, nil
}

func runValidateDocsWithOpts(_ context.Context, opts *validateDocsOpts, stdout, stderr io.Writer) error {
	diags, err := validatedocs.ValidateRepo(opts.repoRoot)
	if err != nil {
		return err
	}

	hasError, hasWarning := classifyDiagnostics(diags)
	failing := hasError || (opts.strict && hasWarning)

	if isJSONOutput(opts.format) {
		result := validateDocsResult{Valid: !failing, Diagnostics: make([]validateJSONDiagnostic, 0, len(diags))}
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
