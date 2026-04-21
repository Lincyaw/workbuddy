package app

import (
	"log"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/security"
)

// AllowSecurityEvent returns true when the given change event should be
// allowed through the state machine given the current security posture.
// Non-author-bound events (poll cycle boundary, issue closed, etc.) are
// always allowed.
func AllowSecurityEvent(secRuntime *security.Runtime, ev poller.ChangeEvent) bool {
	if secRuntime == nil {
		return true
	}
	switch ev.Type {
	case poller.EventIssueCreated, poller.EventLabelAdded, poller.EventLabelRemoved:
	default:
		return true
	}
	current := secRuntime.Current()
	if !current.IsRestricted() || current.Allows(ev.Author) {
		return true
	}
	author := strings.TrimSpace(ev.Author)
	if author == "" {
		author = "unknown"
	}
	log.Printf("[security] skipping issue #%d by @%s: author not in trusted_authors", ev.IssueNum, author)
	return false
}

// LogSecurityPosture logs one line summarizing whether the trusted_authors
// allowlist is active and where the configuration came from.
func LogSecurityPosture(snapshot security.Snapshot) {
	if !snapshot.IsRestricted() {
		log.Printf("[security] trusted_authors: unrestricted (no allowlist configured)")
		return
	}
	log.Printf("[security] trusted_authors: %s (source: %s)", snapshot.FormatAuthors(), snapshot.Source)
}
