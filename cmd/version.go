package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// Set via ldflags at build time.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

type versionOpts struct {
	format string
}

type versionResult struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	RunE:  runVersionCmd,
}

func init() {
	addOutputFormatFlag(versionCmd)
	rootCmd.AddCommand(versionCmd)
}

func runVersionCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseVersionFlags(cmd)
	if err != nil {
		return err
	}
	return runVersionWithOpts(opts, cmd.OutOrStdout())
}

func parseVersionFlags(cmd *cobra.Command) (*versionOpts, error) {
	format, err := resolveOutputFormat(cmd, "version")
	if err != nil {
		return nil, err
	}
	return &versionOpts{format: format}, nil
}

func runVersionWithOpts(opts *versionOpts, stdout io.Writer) error {
	result := versionResult{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
	}
	if isJSONOutput(opts.format) {
		return writeJSON(stdout, result)
	}
	_, err := fmt.Fprintf(stdout, "workbuddy %s (commit %s, built %s)\n", result.Version, result.Commit, result.Date)
	return err
}
