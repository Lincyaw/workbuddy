package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	coordinatorAuthTokenEnvVar   = "WORKBUDDY_AUTH_TOKEN"
	coordinatorAuthTokenFlag     = "token"
	coordinatorAuthTokenFileFlag = "token-file"
)

func addCoordinatorAuthFlags(fs *pflag.FlagSet, shorthand string, usage string) {
	if shorthand != "" {
		fs.StringP(coordinatorAuthTokenFlag, shorthand, "", usage+" (deprecated: use --token-file or WORKBUDDY_AUTH_TOKEN)")
	} else {
		fs.String(coordinatorAuthTokenFlag, "", usage+" (deprecated: use --token-file or WORKBUDDY_AUTH_TOKEN)")
	}
	fs.String(coordinatorAuthTokenFileFlag, "", "Path to a file containing the bearer token for coordinator auth (defaults to WORKBUDDY_AUTH_TOKEN)")
}

func resolveCoordinatorAuthToken(cmd *cobra.Command, scope string) (string, error) {
	token, _ := cmd.Flags().GetString(coordinatorAuthTokenFlag)
	tokenFile, _ := cmd.Flags().GetString(coordinatorAuthTokenFileFlag)
	return resolveCoordinatorAuthTokenValue(cmd.ErrOrStderr(), scope, token, tokenFile)
}

func resolveCoordinatorAuthTokenValue(stderr io.Writer, scope, token, tokenFile string) (string, error) {
	token = strings.TrimSpace(token)
	tokenFile = strings.TrimSpace(tokenFile)

	switch {
	case token != "" && tokenFile != "":
		return "", fmt.Errorf("%s: --token and --token-file cannot be used together", scope)
	case token != "":
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr, "%s: warning: --token is deprecated; use --token-file or %s\n", scope, coordinatorAuthTokenEnvVar)
		}
		return token, nil
	case tokenFile != "":
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("%s: read --token-file %q: %w", scope, tokenFile, err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("%s: --token-file %q is empty", scope, tokenFile)
		}
		return token, nil
	default:
		return strings.TrimSpace(os.Getenv(coordinatorAuthTokenEnvVar)), nil
	}
}
