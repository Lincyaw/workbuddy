package cmd

// Phase 3 of the session-data ownership refactor (REQ-122) replaced the
// legacy worker-side mgmt_base_url HTML proxy with a 302 redirect.
//
// Background: the reporter posts session links of the shape
// `<reportBaseURL>/workers/<workerID>/sessions/<sessionID>` into GitHub
// issue comments (see internal/sessionref). Pre-refactor the coordinator
// proxied that path to the worker's mgmt HTTP server (mgmt_base_url) and
// rewrote in-flight HTML to keep the worker prefix on relative session
// links. Phase 1 added an `audit_url` column on `workers` and Phase 2
// stood up internal/sessionproxy as a JSON proxy keyed off audit_url.
// The HTML viewer the old proxy wrapped is now served by the SPA at
// `/sessions/{id}` directly, reading the same JSON the SPA reads
// elsewhere — the worker mgmt HTML route is no longer the source of
// truth.
//
// We picked option (a) from the Phase 2 ADR: redirect the legacy URL
// shape onto the SPA route. The reporter still emits the
// `/workers/<id>/sessions/<sid>` URL (sessionref.BuildURL), so existing
// GitHub comments keep working — the user lands on the SPA via the
// redirect rather than on a worker-served HTML page. Nothing reads
// mgmt_base_url for sessions any longer; the column is left in place
// for the workers-table response shape (operators inspect it via the
// admin API).
//
// Net effect:
//   * 80 lines of HTML body-rewriting + reverse-proxy machinery deleted.
//   * No coordinator-side dependency on the worker mgmt HTML surface.
//   * GitHub-comment URLs continue to resolve through the SPA, which
//     calls /api/v1/sessions/{id} (sessionproxy → audit_url → worker).

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// newCoordinatorSessionProxy returns an http.Handler that 302-redirects
// `/workers/<id>/sessions/<sid>[suffix]` onto the SPA's canonical
// `/sessions/<sid>[suffix]` route. The redirect preserves any query
// string. Paths outside that shape return 404.
//
// The store and sharedAuth arguments are no longer used (the redirect
// touches neither the DB nor any worker), but the signature is kept so
// the existing wiring in cmd/coordinator.go and the legacy test surface
// compile unchanged.
func newCoordinatorSessionProxy(_ *store.Store, _ string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sessionPath, ok := parseWorkerSessionProxyPath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		// sessionPath is "/sessions/<sid>[/suffix]"; serve as-is.
		target := sessionPath
		if raw := r.URL.RawQuery; raw != "" {
			target += "?" + raw
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
}

// parseWorkerSessionProxyPath parses `/workers/<id>/sessions[/<rest>]`
// into (workerID, "/sessions[/<rest>]", true). Used by the redirect
// handler and exercised by the legacy proxy tests.
func parseWorkerSessionProxyPath(path string) (workerID, sessionPath string, ok bool) {
	if !strings.HasPrefix(path, "/workers/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, "/workers/")
	workerPart, suffix, found := strings.Cut(rest, "/sessions")
	if !found || workerPart == "" {
		return "", "", false
	}
	workerID, err := url.PathUnescape(workerPart)
	if err != nil || strings.TrimSpace(workerID) == "" {
		return "", "", false
	}
	if suffix != "" && !strings.HasPrefix(suffix, "/") {
		return "", "", false
	}
	return workerID, "/sessions" + suffix, true
}
