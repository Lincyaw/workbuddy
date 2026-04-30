// Package app — hooks_api.go exposes the runtime hook surface over HTTP.
//
// Endpoints (registered under /api/v1/hooks/ in the coordinator mux):
//
//   GET  /api/v1/hooks/status — JSON of per-hook stats, last failure, disabled
//   POST /api/v1/hooks/reload — re-read hooks YAML, clear auto-disable, emit
//                                a hooks_reloaded event so dashboards can
//                                correlate.
//
// These are the read/write counterparts of `workbuddy hooks list` (which is
// purely client-side and reads the YAML, not the running dispatcher). Without
// the dispatcher attached the endpoints respond with 503 so operators can
// distinguish "no hooks configured" from "this binary doesn't know about
// hooks".
package app

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/hooks"
)

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

// HandleStatus serves GET /api/v1/hooks/status.
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
