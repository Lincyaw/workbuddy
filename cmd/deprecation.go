package cmd

import (
	"fmt"
	"io"
)

func writeDeprecationWarning(stderr io.Writer, deprecated, replacement string) {
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, "warning: %s is deprecated; use %s instead\n", deprecated, replacement)
}
