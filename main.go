// Package main is the entry point for the workbuddy CLI.
package main

import (
	"fmt"
	"os"

	"github.com/Lincyaw/workbuddy/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		type exitCoder interface {
			ExitCode() int
		}
		if ec, ok := err.(exitCoder); ok {
			if msg := err.Error(); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			os.Exit(ec.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
