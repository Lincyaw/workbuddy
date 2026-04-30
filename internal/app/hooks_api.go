// Package app — hooks_api.go exposes the runtime hook surface over HTTP.
//
// Endpoints (registered under /api/v1/hooks/ in the coordinator mux):
//
//	GET  /api/v1/hooks                     — list registered hooks (webui list view)
//	GET  /api/v1/hooks/status              — same data as /api/v1/hooks; legacy
//	                                         shape kept for `workbuddy hooks status`
//	GET  /api/v1/hooks/{name}/invocations  — recent invocations for one hook
//	                                         (used by the webui timeline)
//	POST /api/v1/hooks/reload              — re-read hooks YAML, clear
//	                                         auto-disable, emit hooks_reloaded
//
// These are the read/write counterparts of `workbuddy hooks list` (which is
// purely client-side and reads the YAML, not the running dispatcher). Without
// the dispatcher attached the endpoints respond with 503 so operators can
// distinguish "no hooks configured" from "this binary doesn't know about
// hooks".
package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/hooks"
)

// invocationsDefaultLimit is the page size for /api/v1/hooks/{name}/invocations
// when the caller doesn't specify one.
const invocationsDefaultLimit = 20

// invocationsMaxLimit caps the page size to the dispatcher's ring-buffer
// length so we never promise more history than we keep.
const invocationsMaxLimit = 100

// HooksAPI is the operational surface for the running dispatcher.
type HooksAPI struct {
	dispatcher *hooks.Dispatcher
	evlog      *eventlog.EventLogger
	configPath string
}

// NewHooksAPI binds the dispatcher and config path. Either may be nil; the
// handlers degrade gracefully (503 when there is nothing to report on).
func NewHooksAPI(d *hooks.Dispatcher, evlog *eventlog.EventLogger, configPath string) *HooksAPI {
	return &HooksAPI{dispatcher: d, evlog: evlog, configPath: configPath}
}

// HookStatusEntry is the JSON projection of one hook's runtime state.
type HookStatusEntry struct {
	Name             string    `json:"name"`
	Events           []string  `json:"events"`
	ActionType       string    `json:"action_type"`
	Enabled          bool      `json:"enabled"`
	AutoDisabled     bool      `json:"auto_disabled"`
	Successes        uint64    `json:"successes"`
	Failures         uint64    `json:"failures"`
	Filtered         uint64    `json:"filtered"`
	DisabledDrops    uint64    `json:"disabled_drops"`
	Overflow         uint64    `json:"overflow"`
	ConsecutiveFails int       `json:"consecutive_failures"`
	LastError        string    `json:"last_error,omitempty"`
	LastFailureAt    time.Time `json:"last_failure_at,omitempty"`
	LastInvokedAt    time.Time `json:"last_invoked_at,omitempty"`
	DurationCount    uint64    `json:"duration_count"`
	DurationSumNs    uint64    `json:"duration_sum_ns"`
}

// HooksStatusResponse is the JSON shape returned by GET /api/v1/hooks/status.
type HooksStatusResponse struct {
	ConfigPath    string            `json:"config_path,omitempty"`
	OverflowTotal uint64            `json:"overflow_total"`
	DroppedTotal  uint64            `json:"dropped_total"`
	Hooks         []HookStatusEntry `json:"hooks"`
}

// HandleStatus serves GET /api/v1/hooks/status (and the canonical
// GET /api/v1/hooks). Both surfaces return the same JSON.
func (h *HooksAPI) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHooksJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if h.dispatcher == nil {
		writeHooksJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "hooks dispatcher not configured"})
		return
	}
	resp := HooksStatusResponse{
		ConfigPath:    h.configPath,
		OverflowTotal: h.dispatcher.OverflowCount(),
		DroppedTotal:  h.dispatcher.DroppedCount(),
	}
	for _, s := range h.dispatcher.Stats() {
		resp.Hooks = append(resp.Hooks, HookStatusEntry{
			Name:             s.Name,
			Events:           s.Events,
			ActionType:       s.ActionType,
			Enabled:          s.Enabled,
			AutoDisabled:     s.Disabled,
			Successes:        s.Successes,
			Failures:         s.Failures,
			Filtered:         s.Filtered,
			DisabledDrops:    s.DisabledDrops,
			Overflow:         s.Overflow,
			ConsecutiveFails: s.ConsecutiveFail,
			LastError:        s.LastError,
			LastFailureAt:    s.LastFailureAt,
			LastInvokedAt:    s.LastInvokedAt,
			DurationCount:    s.DurationCount,
			DurationSumNs:    s.DurationSumNs,
		})
	}
	writeHooksJSON(w, http.StatusOK, resp)
}

// HookInvocationEntry is the JSON projection of one Invocation.
type HookInvocationEntry struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMs int64     `json:"duration_ms"`
	Result     string    `json:"result"`
	Error      string    `json:"error,omitempty"`
	EventType  string    `json:"event_type,omitempty"`
	Repo       string    `json:"repo,omitempty"`
	IssueNum   int       `json:"issue_num,omitempty"`
	Stdout     string    `json:"stdout,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
}

// HookInvocationsResponse is the JSON shape returned by
// GET /api/v1/hooks/{name}/invocations.
type HookInvocationsResponse struct {
	Hook        string                `json:"hook"`
	Limit       int                   `json:"limit"`
	Invocations []HookInvocationEntry `json:"invocations"`
}

// HandleInvocations serves GET /api/v1/hooks/{name}/invocations. The handler
// trims the "/api/v1/hooks/" prefix and "/invocations" suffix to derive the
// hook name so it can be registered with mux.Handle on a subtree.
func (h *HooksAPI) HandleInvocations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHooksJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if h.dispatcher == nil {
		writeHooksJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "hooks dispatcher not configured"})
		return
	}
	name, ok := parseInvocationsPath(r.URL.Path)
	if !ok {
		writeHooksJSON(w, http.StatusBadRequest, map[string]string{"error": "expected /api/v1/hooks/{name}/invocations"})
		return
	}
	limit := invocationsDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeHooksJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a positive integer"})
			return
		}
		if n > invocationsMaxLimit {
			n = invocationsMaxLimit
		}
		limit = n
	}
	invs, found := h.dispatcher.Invocations(name, limit)
	if !found {
		writeHooksJSON(w, http.StatusNotFound, map[string]string{"error": "hook not registered: " + name})
		return
	}
	out := HookInvocationsResponse{Hook: name, Limit: limit, Invocations: make([]HookInvocationEntry, 0, len(invs))}
	for _, inv := range invs {
		out.Invocations = append(out.Invocations, HookInvocationEntry{
			StartedAt:  inv.StartedAt,
			FinishedAt: inv.FinishedAt,
			DurationMs: inv.DurationNs / int64(time.Millisecond),
			Result:     inv.Result,
			Error:      inv.Error,
			EventType:  inv.EventType,
			Repo:       inv.Repo,
			IssueNum:   inv.IssueNum,
			Stdout:     inv.Stdout,
			Stderr:     inv.Stderr,
		})
	}
	writeHooksJSON(w, http.StatusOK, out)
}

// parseInvocationsPath extracts the hook name from a path of the form
// /api/v1/hooks/{name}/invocations. Returns ok=false on any other shape so
// the handler can 400 with a stable message.
func parseInvocationsPath(p string) (string, bool) {
	const prefix = "/api/v1/hooks/"
	const suffix = "/invocations"
	if !strings.HasPrefix(p, prefix) || !strings.HasSuffix(p, suffix) {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(p, prefix), suffix)
	name = strings.Trim(name, "/")
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// HookConfigResponse is the JSON shape returned by GET /api/v1/hooks/{name}/config.
// Body is the raw YAML so the webui can show the operator the source of truth
// without trying to round-trip it through structured types.
type HookConfigResponse struct {
	Hook       string `json:"hook"`
	ConfigPath string `json:"config_path,omitempty"`
	YAML       string `json:"yaml,omitempty"`
	Note       string `json:"note,omitempty"`
}

// HandleConfig serves GET /api/v1/hooks/{name}/config. Returns the entire
// hooks YAML (the file is the unit of truth) so the operator can see comments
// and ordering. Returns 404 only when the hook is not bound at all.
func (h *HooksAPI) HandleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHooksJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if h.dispatcher == nil {
		writeHooksJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "hooks dispatcher not configured"})
		return
	}
	name, ok := parseSimpleHookPath(r.URL.Path, "/config")
	if !ok {
		writeHooksJSON(w, http.StatusBadRequest, map[string]string{"error": "expected /api/v1/hooks/{name}/config"})
		return
	}
	if _, found := h.dispatcher.Invocations(name, 1); !found {
		writeHooksJSON(w, http.StatusNotFound, map[string]string{"error": "hook not registered: " + name})
		return
	}
	resp := HookConfigResponse{Hook: name, ConfigPath: h.configPath}
	if h.configPath == "" {
		resp.Note = "no config path bound to dispatcher (dispatcher built from in-memory config)"
		writeHooksJSON(w, http.StatusOK, resp)
		return
	}
	data, err := os.ReadFile(h.configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			resp.Note = "config file no longer exists at " + h.configPath
			writeHooksJSON(w, http.StatusOK, resp)
			return
		}
		writeHooksJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp.YAML = string(data)
	writeHooksJSON(w, http.StatusOK, resp)
}

// parseSimpleHookPath extracts {name} from /api/v1/hooks/{name}<suffix>.
func parseSimpleHookPath(p, suffix string) (string, bool) {
	const prefix = "/api/v1/hooks/"
	if !strings.HasPrefix(p, prefix) || !strings.HasSuffix(p, suffix) {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(p, prefix), suffix)
	name = strings.Trim(name, "/")
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// HandleHookSubtree dispatches /api/v1/hooks/{name}/<sub> requests. Mounted
// once on the /api/v1/hooks/ subtree so existing exact patterns
// (/status, /reload) still win and we don't have to register one mux entry
// per per-hook resource.
func (h *HooksAPI) HandleHookSubtree(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/invocations"):
		h.HandleInvocations(w, r)
	case strings.HasSuffix(r.URL.Path, "/config"):
		h.HandleConfig(w, r)
	default:
		writeHooksJSON(w, http.StatusNotFound, map[string]string{"error": "unknown hook subresource: " + r.URL.Path})
	}
}

// HooksReloadResponse is the JSON shape returned by POST /api/v1/hooks/reload.
type HooksReloadResponse struct {
	ConfigPath string   `json:"config_path"`
	HookCount  int      `json:"hook_count"`
	Warnings   []string `json:"warnings,omitempty"`
}

// HandleReload serves POST /api/v1/hooks/reload. Reload re-reads the YAML,
// rebuilds the dispatcher's hook bindings (clearing auto-disable along the
// way), and emits a hooks_reloaded event so external consumers can
// correlate the action with subsequent successes/failures.
func (h *HooksAPI) HandleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeHooksJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if h.dispatcher == nil {
		writeHooksJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "hooks dispatcher not configured"})
		return
	}
	cfg, parseWarnings, err := hooks.LoadConfig(h.configPath)
	if err != nil {
		writeHooksJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	buildWarnings, err := h.dispatcher.Reload(cfg, hooks.DefaultActionRegistry())
	if err != nil {
		writeHooksJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	all := append([]string(nil), parseWarnings...)
	all = append(all, buildWarnings...)
	count := 0
	if cfg != nil {
		for i := range cfg.Hooks {
			if cfg.Hooks[i].IsEnabled() {
				count++
			}
		}
	}
	if h.evlog != nil {
		h.evlog.Log(eventlog.TypeHooksReloaded, "", 0, map[string]interface{}{
			"config_path": h.configPath,
			"hook_count":  count,
			"warnings":    all,
		})
	}
	writeHooksJSON(w, http.StatusOK, HooksReloadResponse{
		ConfigPath: h.configPath,
		HookCount:  count,
		Warnings:   all,
	})
}

func writeHooksJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
