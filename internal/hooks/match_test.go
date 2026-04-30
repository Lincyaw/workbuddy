package hooks

import (
	"strings"
	"testing"
)

// Severity filter on a non-alert event must be flagged at parse time so the
// operator notices the misconfig at boot rather than getting silently ignored
// at runtime.
func TestParseConfigWarnsOnSeverityForNonAlert(t *testing.T) {
	yaml := []byte(`hooks:
  - name: bad
    events: [report]
    match:
      severity: [warning]
    action:
      type: webhook
      url: https://example.invalid/hook
`)
	_, warnings, err := ParseConfig(yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "match.severity only applies to event_type=alert") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected severity warning, got %v", warnings)
	}
}

func TestParseConfigNoSeverityWarningOnAlert(t *testing.T) {
	yaml := []byte(`hooks:
  - name: ok
    events: [alert]
    match:
      severity: [warning, critical]
    action:
      type: webhook
      url: https://example.invalid/hook
`)
	_, warnings, err := ParseConfig(yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "match.severity") {
			t.Fatalf("did not expect severity warning, got %v", warnings)
		}
	}
}

func TestParseConfigRejectsInvalidRepoGlob(t *testing.T) {
	// path.Match treats `[` as a character-class opener; an unclosed class is
	// ErrBadPattern.
	yaml := []byte(`hooks:
  - name: bad
    events: [alert]
    match:
      repo: "owner/[unclosed"
    action:
      type: webhook
      url: https://example.invalid/hook
`)
	_, _, err := ParseConfig(yaml)
	if err == nil {
		t.Fatalf("expected invalid repo glob error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid match.repo glob") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMatchesFilterRepoGlob(t *testing.T) {
	enabled := true
	h := &Hook{
		Name:    "g",
		Events:  []string{"alert"},
		Enabled: &enabled,
		Match:   &MatchFilter{Repo: "owner/*"},
	}
	cases := []struct {
		repo    string
		matched bool
	}{
		{"owner/repo1", true},
		{"owner/anything", true},
		{"other/repo", false},
		{"", false},
	}
	for _, c := range cases {
		if got := h.MatchesFilter(Event{Type: "alert", Repo: c.repo}); got != c.matched {
			t.Errorf("repo=%q got %v want %v", c.repo, got, c.matched)
		}
	}
}

func TestMatchesFilterSeverityAlertOnly(t *testing.T) {
	h := &Hook{
		Name:   "s",
		Events: []string{"alert"},
		Match:  &MatchFilter{Severity: []string{"warning", "critical"}},
	}
	// Hits.
	if !h.MatchesFilter(Event{Type: "alert", Payload: []byte(`{"severity":"warning"}`)}) {
		t.Fatalf("severity warning should match")
	}
	if !h.MatchesFilter(Event{Type: "alert", Payload: []byte(`{"severity":"CRITICAL"}`)}) {
		t.Fatalf("severity should be case-insensitive")
	}
	// Misses.
	if h.MatchesFilter(Event{Type: "alert", Payload: []byte(`{"severity":"info"}`)}) {
		t.Fatalf("severity info should NOT match")
	}
	if h.MatchesFilter(Event{Type: "alert", Payload: []byte(`{}`)}) {
		t.Fatalf("missing severity field should NOT match alert filter")
	}
	// Severity is a no-op on non-alert events (warning at config time).
	if !h.MatchesFilter(Event{Type: "report", Payload: []byte(`{"severity":"info"}`)}) {
		t.Fatalf("severity filter must not gate non-alert events at runtime")
	}
}

func TestMatchesFilterEmptyIsMatchAll(t *testing.T) {
	h := &Hook{Events: []string{"*"}} // no Match block at all
	if !h.MatchesFilter(Event{Type: "alert", Repo: "x/y"}) {
		t.Fatalf("nil match must be match-all")
	}
}
