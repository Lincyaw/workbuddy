package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	intvalidate "github.com/Lincyaw/workbuddy/internal/validate"
	"github.com/spf13/cobra"
)

type validateOpts struct {
	configDir string
}

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate workbuddy config files and workflow references",
	RunE:  runValidateCmd,
}

func init() {
	validateCmd.Flags().String("config-dir", ".github/workbuddy", "Configuration directory to validate")
	rootCmd.AddCommand(validateCmd)
}

func runValidateCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseValidateFlags(cmd)
	if err != nil {
		return err
	}
	return runValidateWithOpts(cmd.Context(), opts, os.Stdout, os.Stderr)
}

func parseValidateFlags(cmd *cobra.Command) (*validateOpts, error) {
	configDir, _ := cmd.Flags().GetString("config-dir")
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return nil, fmt.Errorf("validate: --config-dir is required")
	}
	return &validateOpts{configDir: configDir}, nil
}

func runValidateWithOpts(_ context.Context, opts *validateOpts, stdout, stderr io.Writer) error {
	diags, err := intvalidate.ValidateDir(opts.configDir)
	if err != nil {
		return err
	}
	if len(diags) == 0 {
		return nil
	}
	for _, diag := range diags {
		_, _ = fmt.Fprintln(stderr, diag.String())
	}
	return &cliExitError{code: 1}
}
