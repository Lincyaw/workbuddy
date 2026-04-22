package cmd

import (
	"fmt"
	"strings"
)

func actionableError(context, cause, suggestion string) error {
	context = strings.TrimSpace(context)
	cause = strings.TrimSpace(strings.TrimSuffix(cause, "."))
	suggestion = strings.TrimSpace(strings.TrimSuffix(suggestion, "."))

	if suggestion == "" {
		return fmt.Errorf("%s: %s", context, cause)
	}
	return fmt.Errorf("%s: %s. %s.", context, cause, suggestion)
}
