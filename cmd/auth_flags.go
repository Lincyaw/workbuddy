package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	coordinatorAuthTokenEnvVar   = "WORKBUDDY_AUTH_TOKEN"
	coordinatorAuthTokenFileFlag = "token-file"
)

func addCoordinatorAuthFlags(fs *pflag.FlagSet, _ string, usage string) {
	fs.String(coordinatorAuthTokenFileFlag, "", "Path to a file containing the bearer token for coordinator auth (defaults to WORKBUDDY_AUTH_TOKEN)")
}

func resolveCoordinatorAuthToken(cmd *cobra.Command, scope string) (string, error) {
	tokenFile, _ := cmd.Flags().GetString(coordinatorAuthTokenFileFlag)
	return resolveCoordinatorAuthTokenValue(tokenFile)
}

func resolveCoordinatorAuthTokenValue(tokenFile string) (string, error) {
	tokenFile = strings.TrimSpace(tokenFile)

	if tokenFile != "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("read --token-file %q: %w", tokenFile, err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("--token-file %q is empty", tokenFile)
		}
		return token, nil
	}
	return strings.TrimSpace(os.Getenv(coordinatorAuthTokenEnvVar)), nil
}
