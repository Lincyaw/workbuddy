// Package hooks implements the operator-owned event hook system described in
// docs/decisions/2026-04-30-hook-system.md.
//
// Phase 1a scope (issue #264): YAML config loader, dispatcher with bounded
// channel + per-hook workers, ActionRegistry, webhook action, and stable v1
// event payload envelope. command action, timeouts, auto-disable, and match
// filters are out of scope and land in later phases.
package hooks

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ConfigSchemaVersion is the only supported top-level schema_version.
const ConfigSchemaVersion = 1

// Config is the parsed form of ~/.config/workbuddy/hooks.yaml.
type Config struct {
	SchemaVersion int    `yaml:"schema_version"`
	Hooks         []Hook `yaml:"hooks"`
}

// Hook is one declarative event → action binding.
type Hook struct {
	Name    string        `yaml:"name"`
	Enabled *bool         `yaml:"enabled,omitempty"`
	Events  []string      `yaml:"events"`
	Match   *MatchFilter  `yaml:"match,omitempty"`
	Timeout time.Duration `yaml:"-"`
	RawTimeout string     `yaml:"timeout,omitempty"`
	Action  ActionConfig  `yaml:"action"`
}

// MatchFilter narrows the events a hook receives. Filters AND together: an
// event must satisfy every set filter to be delivered.
//
//   - Severity is honoured only when the event_type is `alert`. Configuring it
//     for any other event surfaces a startup warning (see ParseConfig).
//   - Repo is a glob (path.Match syntax) tested against the event's repo
//     field. Empty/missing repo filter is match-all.
type MatchFilter struct {
	Severity []string `yaml:"severity,omitempty"`
	Repo     string   `yaml:"repo,omitempty"`
}

// ActionConfig holds the raw YAML for an action; the concrete instance is
// constructed by the ActionRegistry at load time.
type ActionConfig struct {
	Type    string         `yaml:"type"`
	URL     string         `yaml:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Method  string         `yaml:"method,omitempty"`
	Cmd     []string       `yaml:"cmd,omitempty"`
	Cwd     string         `yaml:"cwd,omitempty"`
}

// IsEnabled returns the effective enabled flag (default true).
func (h *Hook) IsEnabled() bool {
	if h == nil {
		return false
	}
	if h.Enabled == nil {
		return true
	}
	return *h.Enabled
}

// LoadConfig reads and validates a hooks YAML file. The returned warnings are
// non-fatal (e.g. unresolved env vars in headers) and are intended for the
// caller to log at startup. A missing file returns (nil, nil, nil) so callers
// can treat "no hook config" as valid.
func LoadConfig(path string) (*Config, []string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("hooks: read %s: %w", path, err)
	}
	return ParseConfig(raw)
}

// ParseConfig parses a YAML byte slice. Exposed for tests.
func ParseConfig(raw []byte) (*Config, []string, error) {
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, nil, fmt.Errorf("hooks: parse yaml: %w", err)
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = ConfigSchemaVersion
	}
	if cfg.SchemaVersion != ConfigSchemaVersion {
		return nil, nil, fmt.Errorf("hooks: unsupported schema_version %d (want %d)", cfg.SchemaVersion, ConfigSchemaVersion)
	}

	var warnings []string
	seen := map[string]struct{}{}
	for i := range cfg.Hooks {
		h := &cfg.Hooks[i]
		if err := validateHook(h, seen); err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(h.RawTimeout) != "" {
			d, err := time.ParseDuration(h.RawTimeout)
			if err != nil {
				return nil, nil, fmt.Errorf("hooks: hook %q: invalid timeout %q: %w", h.Name, h.RawTimeout, err)
			}
			h.Timeout = d
		}
		w, err := finalizeAction(h)
		if err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, w...)
		if mw, err := validateMatchFilter(h); err != nil {
			return nil, nil, err
		} else {
			warnings = append(warnings, mw...)
		}
	}
	return &cfg, warnings, nil
}

// validateMatchFilter returns startup warnings (e.g. severity on non-alert
// events) and an error if the filter is structurally invalid (e.g. malformed
// repo glob).
func validateMatchFilter(h *Hook) ([]string, error) {
	if h == nil || h.Match == nil {
		return nil, nil
	}
	var warnings []string
	if len(h.Match.Severity) > 0 {
		// severity only applies when the hook subscribes solely to alert
		// events. If the event list contains anything non-alert (or `*`),
		// the filter cannot be enforced for those — surface a warning.
		nonAlert := false
		for _, ev := range h.Events {
			if ev != "alert" {
				nonAlert = true
				break
			}
		}
		if nonAlert {
			warnings = append(warnings, fmt.Sprintf("hook %q: match.severity only applies to event_type=alert; ignored for other events", h.Name))
		}
	}
	if strings.TrimSpace(h.Match.Repo) != "" {
		// Validate the glob early — path.Match returns ErrBadPattern only when
		// the pattern is matched, so probe with an arbitrary input.
		if _, err := path.Match(h.Match.Repo, "owner/repo"); err != nil {
			return nil, fmt.Errorf("hooks: hook %q: invalid match.repo glob %q: %w", h.Name, h.Match.Repo, err)
		}
	}
	return warnings, nil
}

// MatchesEvelope reports whether the hook's match filter accepts the event.
// The dispatcher calls this after the event-type subscription check; the
// filter is the second-stage gate.
//
// Semantics:
//   - severity: applies only to event_type=alert. The payload must carry a
//     top-level `severity` JSON string and it must appear in the allow-list
//     (case-insensitive). On non-alert events this filter is a no-op (with a
//     startup warning surfaced via ParseConfig).
//   - repo: glob via path.Match against the event's Repo field. Empty filter
//     matches everything (including events with no repo).
func (h *Hook) MatchesFilter(ev Event) bool {
	if h == nil || h.Match == nil {
		return true
	}
	if h.Match.Repo != "" {
		ok, err := path.Match(h.Match.Repo, ev.Repo)
		if err != nil || !ok {
			return false
		}
	}
	if len(h.Match.Severity) > 0 && ev.Type == "alert" {
		sev := extractSeverity(ev.Payload)
		if sev == "" {
			return false
		}
		matched := false
		for _, allowed := range h.Match.Severity {
			if strings.EqualFold(strings.TrimSpace(allowed), sev) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// extractSeverity pulls the `severity` string from a JSON payload using a
// shallow regex so we don't need to fully unmarshal every event. Returns
// the empty string when the field is absent or malformed.
var severityFieldPattern = regexp.MustCompile(`"severity"\s*:\s*"([^"]*)"`)

func extractSeverity(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	m := severityFieldPattern.FindSubmatch(payload)
	if len(m) < 2 {
		return ""
	}
	return string(m[1])
}

var hookNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func validateHook(h *Hook, seen map[string]struct{}) error {
	if strings.TrimSpace(h.Name) == "" {
		return fmt.Errorf("hooks: hook missing name")
	}
	if !hookNamePattern.MatchString(h.Name) {
		return fmt.Errorf("hooks: hook %q: name must match %s", h.Name, hookNamePattern.String())
	}
	if _, dup := seen[h.Name]; dup {
		return fmt.Errorf("hooks: duplicate hook name %q", h.Name)
	}
	seen[h.Name] = struct{}{}
	if len(h.Events) == 0 {
		return fmt.Errorf("hooks: hook %q: events is required", h.Name)
	}
	for _, ev := range h.Events {
		if strings.TrimSpace(ev) == "" {
			return fmt.Errorf("hooks: hook %q: empty event entry", h.Name)
		}
	}
	if strings.TrimSpace(h.Action.Type) == "" {
		return fmt.Errorf("hooks: hook %q: action.type is required", h.Name)
	}
	return nil
}

func finalizeAction(h *Hook) ([]string, error) {
	switch h.Action.Type {
	case ActionTypeWebhook:
		return finalizeWebhookAction(h)
	case ActionTypeCommand:
		return finalizeCommandAction(h)
	default:
		return nil, fmt.Errorf("hooks: hook %q: unknown action type %q", h.Name, h.Action.Type)
	}
}

// MatchesEvent reports whether the hook subscribes to the given event type.
// `*` matches every event except those filtered out by the dispatcher's
// hook_* prefix guard (so `*` is safe — it won't self-amplify).
func (h *Hook) MatchesEvent(eventType string) bool {
	for _, ev := range h.Events {
		if ev == "*" || ev == eventType {
			return true
		}
	}
	return false
}
