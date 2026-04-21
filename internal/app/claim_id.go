package app

import (
	"fmt"
	"os"
	"strings"
)

// HostnameOrUnknown returns the OS hostname, or the literal string "unknown"
// when the hostname is not available. Used to build stable, process-scoped
// claimer IDs for the per-issue dispatch claim.
func HostnameOrUnknown() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "unknown"
	}
	return hostname
}

// BuildIssueClaimerID returns a claimer ID that is unique per OS process, so
// two coordinators on the same host never collapse onto the same logical
// owner of an issue claim.
func BuildIssueClaimerID(base string, pid int) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "coordinator-" + HostnameOrUnknown()
	}
	if pid <= 0 {
		pid = os.Getpid()
	}
	return fmt.Sprintf("%s-pid-%d", base, pid)
}
