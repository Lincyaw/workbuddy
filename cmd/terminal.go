package cmd

import (
	"os"

	"github.com/mattn/go-isatty"
)

var commandIsInteractiveTerminal = defaultCommandIsInteractiveTerminal

func defaultCommandIsInteractiveTerminal() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}
