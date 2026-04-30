package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/hooks"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// freshStore opens a temp SQLite store for tests. Returned cleanup runs the
// store.Close on test end.
func freshStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dbPath
}

// Reload via HTTP must emit a hooks_reloaded event with the config path so
// dashboards can correlate the reload with subsequent activity.
func TestHooksAPIReloadEmitsEvent(t *testing.T) {
	st, _ := freshStore(t)
	evlog := eventlog.NewEventLogger(st)

	yaml := []byte(`hooks:
  - name: t
    events: [alert]
    action:
      type: webhook
      url: https://example.invalid/hook
`)
	cfgPath := filepath.Join(t.TempDir(), "hooks.yaml")
	if err := os.WriteFile(cfgPath, yaml, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := hooks.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	d, _, err := hooks.NewDispatcher(cfg, hooks.DefaultActionRegistry())
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	api := NewHooksAPI(d, evlog, cfgPath)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/reload", nil)
	rr := httptest.NewRecorder()
	api.HandleReload(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
	var resp HooksReloadResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.HookCount != 1 {
		t.Fatalf("hook_count=%d want 1", resp.HookCount)
	}

	// hooks_reloaded event must be in the eventlog.
	deadline := time.Now().Add(2 * time.Second)
	var got []store.Event
	for time.Now().Before(deadline) {
		got, _ = evlog.Query(eventlog.EventFilter{Type: eventlog.TypeHooksReloaded})
		if len(got) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(got) == 0 {
		t.Fatalf("hooks_reloaded event not recorded")
	}
	if !strings.Contains(got[0].Payload, "hook_count") {
		t.Fatalf("payload missing hook_count: %s", got[0].Payload)
	}
}

func TestHooksAPIStatusReportsCounters(t *testing.T) {
	st, _ := freshStore(t)
	evlog := eventlog.NewEventLogger(st)

	enabled := true
	cfg := &hooks.Config{
		SchemaVersion: 1,
		Hooks: []hooks.Hook{{
			Name:    "t",
			Enabled: &enabled,
			Events:  []string{"alert"},
			Action:  hooks.ActionConfig{Type: "webhook", URL: "https://example.invalid/hook"},
		}},
	}
	d, _, err := hooks.NewDispatcher(cfg, hooks.DefaultActionRegistry())
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	api := NewHooksAPI(d, evlog, "/etc/workbuddy/hooks.yaml")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hooks/status", nil)
	rr := httptest.NewRecorder()
	api.HandleStatus(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
	var resp HooksStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ConfigPath != "/etc/workbuddy/hooks.yaml" {
		t.Fatalf("config_path=%q", resp.ConfigPath)
	}
	if len(resp.Hooks) != 1 || resp.Hooks[0].Name != "t" {
		t.Fatalf("unexpected hooks: %+v", resp.Hooks)
	}
}

func TestHooksAPIStatusReturns503WhenDispatcherMissing(t *testing.T) {
	api := NewHooksAPI(nil, nil, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hooks/status", nil)
	rr := httptest.NewRecorder()
	api.HandleStatus(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rr.Code)
	}
}

func TestHooksAPIInvocationsReturnsRecentEntries(t *testing.T) {
	enabled := true
	cfg := &hooks.Config{
		SchemaVersion: 1,
		Hooks: []hooks.Hook{{
			Name:    "h",
			Enabled: &enabled,
			Events:  []string{"alert"},
			Action:  hooks.ActionConfig{Type: "webhook", URL: "https://example.invalid/hook"},
		}},
	}
	d, _, err := hooks.NewDispatcher(cfg, hooks.DefaultActionRegistry())
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	api := NewHooksAPI(d, nil, "")
	// The dispatcher hasn't run anything, so the buffer is empty — we still
	// expect 200 with an empty invocations array (stable contract for the UI).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hooks/h/invocations?limit=5", nil)
	rr := httptest.NewRecorder()
	api.HandleInvocations(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp HookInvocationsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Hook != "h" {
		t.Fatalf("hook=%q", resp.Hook)
	}
	if resp.Limit != 5 {
		t.Fatalf("limit=%d want 5", resp.Limit)
	}
	if resp.Invocations == nil {
		t.Fatalf("invocations should be a non-nil empty slice for stable JSON shape")
	}
}

func TestHooksAPIInvocationsUnknownHookReturns404(t *testing.T) {
	cfg := &hooks.Config{SchemaVersion: 1}
	d, _, err := hooks.NewDispatcher(cfg, hooks.DefaultActionRegistry())
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	api := NewHooksAPI(d, nil, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hooks/missing/invocations", nil)
	rr := httptest.NewRecorder()
	api.HandleInvocations(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rr.Code)
	}
}

func TestHooksAPIInvocationsLimitValidation(t *testing.T) {
	enabled := true
	cfg := &hooks.Config{
		SchemaVersion: 1,
		Hooks: []hooks.Hook{{
			Name:    "h",
			Enabled: &enabled,
			Events:  []string{"alert"},
			Action:  hooks.ActionConfig{Type: "webhook", URL: "https://example.invalid/hook"},
		}},
	}
	d, _, _ := hooks.NewDispatcher(cfg, hooks.DefaultActionRegistry())
	api := NewHooksAPI(d, nil, "")
	for _, bad := range []string{"abc", "0", "-1"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/hooks/h/invocations?limit="+bad, nil)
		rr := httptest.NewRecorder()
		api.HandleInvocations(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("limit=%q status=%d want 400", bad, rr.Code)
		}
	}
}

func TestHooksAPIConfigReturnsYAMLContents(t *testing.T) {
	enabled := true
	yaml := []byte("hooks:\n  - name: h\n    events: [alert]\n    action:\n      type: webhook\n      url: https://example.invalid/hook\n")
	cfgPath := filepath.Join(t.TempDir(), "hooks.yaml")
	if err := os.WriteFile(cfgPath, yaml, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := &hooks.Config{
		SchemaVersion: 1,
		Hooks: []hooks.Hook{{
			Name:    "h",
			Enabled: &enabled,
			Events:  []string{"alert"},
			Action:  hooks.ActionConfig{Type: "webhook", URL: "https://example.invalid/hook"},
		}},
	}
	d, _, _ := hooks.NewDispatcher(cfg, hooks.DefaultActionRegistry())
	api := NewHooksAPI(d, nil, cfgPath)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hooks/h/config", nil)
	rr := httptest.NewRecorder()
	api.HandleConfig(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp HookConfigResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.YAML, "name: h") {
		t.Fatalf("yaml not echoed back: %q", resp.YAML)
	}
	if resp.ConfigPath != cfgPath {
		t.Fatalf("config_path=%q", resp.ConfigPath)
	}
}

func TestHooksAPISubtreeRoutesPerHookResource(t *testing.T) {
	enabled := true
	cfg := &hooks.Config{
		SchemaVersion: 1,
		Hooks: []hooks.Hook{{
			Name:    "h",
			Enabled: &enabled,
			Events:  []string{"alert"},
			Action:  hooks.ActionConfig{Type: "webhook", URL: "https://example.invalid/hook"},
		}},
	}
	d, _, _ := hooks.NewDispatcher(cfg, hooks.DefaultActionRegistry())
	api := NewHooksAPI(d, nil, "")

	// /api/v1/hooks/h/invocations must reach HandleInvocations through the
	// subtree dispatcher (HandleHookSubtree).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hooks/h/invocations", nil)
	rr := httptest.NewRecorder()
	api.HandleHookSubtree(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("invocations: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Unknown subresource → 404.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/hooks/h/garbage", nil)
	rr = httptest.NewRecorder()
	api.HandleHookSubtree(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown subresource: status=%d", rr.Code)
	}
}
