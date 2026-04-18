package cmd

import (
	"fmt"
	"os"
	"strings"
)

func buildIssueClaimerID(base string, pid int) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "coordinator-" + hostnameOrUnknown()
	}
	if pid <= 0 {
		pid = os.Getpid()
	}
	return fmt.Sprintf("%s-pid-%d", base, pid)
}
