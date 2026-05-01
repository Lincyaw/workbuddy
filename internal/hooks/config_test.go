package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigMissingFileIsSoftNoOp(t *testing.T) {
	dir := t.TempDir()
	cfg, warnings, err := LoadConfig(filepath.Join(dir, "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config, got %+v", cfg)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}

func TestParseConfigValidWebhook(t *testing.T) {
	yaml := []byte(`
schema_version: 1
hooks:
  - name: slack-alerts
    events: [alert, dispatch]
    timeout: 3s
    action:
      type: webhook
      url: https://example.com/hook
      headers:
        X-Token: "${HOOKS_TEST_TOKEN}"
`)
	t.Setenv("HOOKS_TEST_TOKEN", "secret-value")
	cfg, warnings, err := ParseConfig(yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
	if len(cfg.Hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(cfg.Hooks))
	}
	h := cfg.Hooks[0]
	if h.Name != "slack-alerts" || h.Action.Type != ActionTypeWebhook {
		t.Fatalf("hook fields wrong: %+v", h)
	}
	if got := h.Action.Headers["X-Token"]; got != "${HOOKS_TEST_TOKEN}" {
		// resolveHeaders is called inside finalizeAction; raw map preserved
		// untouched in cfg, but resolution happens at action build time.
		// Re-run via builder to assert the real outcome.
		_ = got
	}
	action, _, err := DefaultActionRegistry().Build(&h)
	if err != nil {
		t.Fatalf("build action: %v", err)
	}
	w := action.(*WebhookAction)
	if got := w.headers["X-Token"]; got != "secret-value" {
		t.Fatalf("env var substitution failed: got %q", got)
	}
}

func TestParseConfigMissingFields(t *testing.T) {
	cases := map[string]string{
		"missing name":   `hooks: [{events: [alert], action: {type: webhook, url: http://x}}]`,
		"missing events": `hooks: [{name: a, action: {type: webhook, url: http://x}}]`,
		"missing action": `hooks: [{name: a, events: [alert]}]`,
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			if _, _, err := ParseConfig([]byte(body)); err == nil {
				t.Fatalf("expected error for %s", label)
			}
		})
	}
}

func TestParseConfigUnknownActionType(t *testing.T) {
	yaml := []byte(`hooks: [{name: x, events: [alert], action: {type: smtp}}]`)
	_, _, err := ParseConfig(yaml)
	if err == nil || !strings.Contains(err.Error(), "unknown action type") {
		t.Fatalf("expected unknown-action error, got %v", err)
	}
}

func TestParseConfigCommandActionAccepted(t *testing.T) {
	yaml := []byte(`hooks: [{name: x, events: [alert], action: {type: command, cmd: ["/bin/true"]}}]`)
	cfg, _, err := ParseConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("expected command to be accepted, got %v", err)
	}
	if cfg.Hooks[0].Action.Type != ActionTypeCommand {
		t.Fatalf("expected command action, got %q", cfg.Hooks[0].Action.Type)
	}
}

func TestParseConfigCommandRequiresCmd(t *testing.T) {
	yaml := []byte(`hooks: [{name: x, events: [alert], action: {type: command}}]`)
	_, _, err := ParseConfig(yaml)
	if err == nil || !strings.Contains(err.Error(), "command.cmd") {
		t.Fatalf("expected command.cmd error, got %v", err)
	}
}

func TestParseConfigDuplicateName(t *testing.T) {
	yaml := []byte(`
hooks:
  - {name: dup, events: [alert], action: {type: webhook, url: http://a}}
  - {name: dup, events: [dispatch], action: {type: webhook, url: http://b}}
`)
	if _, _, err := ParseConfig(yaml); err == nil {
		t.Fatalf("expected duplicate-name error")
	}
}

func TestResolveHeadersWarnsOnMissingEnv(t *testing.T) {
	t.Setenv("HOOKS_TEST_PRESENT", "ok")
	resolved, warnings := resolveHeaders("h", map[string]string{
		"A": "${HOOKS_TEST_PRESENT}",
		"B": "${HOOKS_TEST_DEFINITELY_NOT_SET_42}",
	})
	if resolved["A"] != "ok" {
		t.Fatalf("present env not substituted: %v", resolved)
	}
	if resolved["B"] != "" {
		t.Fatalf("missing env should resolve to empty string, got %q", resolved["B"])
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "HOOKS_TEST_DEFINITELY_NOT_SET_42") {
		t.Fatalf("expected warning for missing env, got %v", warnings)
	}
}

func TestLoadConfigFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.yaml")
	if err := os.WriteFile(path, []byte(`
hooks:
  - name: dev-test
    events: ["*"]
    action:
      type: webhook
      url: http://localhost:1234
`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, _, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Hooks) != 1 || !cfg.Hooks[0].MatchesEvent("anything") {
		t.Fatalf("wildcard hook missing: %+v", cfg)
	}
}

func TestHookMatchesEventGlob(t *testing.T) {
	hook := Hook{Events: []string{"rollout_*"}}
	if !hook.MatchesEvent("rollout_group_started") {
		t.Fatalf("expected rollout_* to match rollout_group_started")
	}
	if !hook.MatchesEvent("rollout_group_resolved") {
		t.Fatalf("expected rollout_* to match rollout_group_resolved")
	}
	if hook.MatchesEvent("dispatch") {
		t.Fatalf("did not expect rollout_* to match dispatch")
	}
}
