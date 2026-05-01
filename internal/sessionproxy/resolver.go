// Package sessionproxy implements the coordinator-side session API as a
// reverse-proxy / fan-out front for the worker audit servers introduced in
// Phase 1 of the session-data ownership refactor (REQ-120,
// docs/decisions/2026-05-01-session-data-ownership.md).
//
// In the v0.5 bundle topology the worker owns its own SQLite DB and the
// per-session artefact directory. The coordinator's local DB has no session
// rows for anything created since the cutover. Reading from the coordinator's
// local store returns a stale or empty picture; the legacy disk-only
// synthesis fallback then mislabels healthy workers' sessions as
// `aborted_before_start`.
//
// This package translates inbound coordinator session requests into outbound
// requests against the owning worker's `audit_url` (advertised at register
// time and persisted on `workers.audit_url`). The bearer token is forwarded
// as Authorization. Browser disconnects propagate; per-worker errors degrade
// gracefully into a 503 / `worker_offline` envelope.
//
// Phase 3 hook points are flagged inline.
package sessionproxy

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// Sentinel errors returned by Resolve. Callers turn these into
// 404 / 503 / fall-back-local responses.
var (
	// ErrSessionNotRouted means the coordinator's local DB has no row
	// linking session_id to a worker — either the session belongs to a
	// pre-bundle deployment whose data lives only on the coordinator's own
	// disk, or the session_id is bogus.
	ErrSessionNotRouted = errors.New("sessionproxy: session not routed")
	// ErrAuditURLMissing is retained for the fan-out listing path when a
	// candidate worker has no audit_url AND no local handler is available
	// to fall back to. The single-session resolver no longer returns this:
	// an empty audit_url instead degrades to Resolution{Local: true} so the
	// in-process serve topology and old-worker-without-audit-listen
	// rollouts both succeed via the coordinator-local fallback.
	ErrAuditURLMissing = errors.New("sessionproxy: worker has no audit_url")
	// ErrWorkerOffline is reserved for the proxy/fan-out layers — the
	// resolver itself does not call workers, so it does not return this
	// sentinel; the proxy promotes a dial / non-2xx error to ErrWorkerOffline
	// when wiring the response.
	ErrWorkerOffline = errors.New("sessionproxy: worker offline")
)

// Resolution is the output of Resolve.
type Resolution struct {
	WorkerID string
	// AuditURL is the worker-advertised base URL (no trailing slash).
	// Empty when the resolver decides the request should be served
	// locally (see Local).
	AuditURL string
	// Local is true when the request should fall through to the
	// coordinator's local audit handler instead of being proxied. Two
	// situations trigger this:
	//   - the resolved audit_url points at the coordinator's own host
	//     (loopback / hostname match / explicit --local-audit-fallback
	//     opt-in);
	//   - no audit_url is advertised and a coordinator-local sessionsDir
	//     can still serve the request from disk.
	// Phase 3 will likely delete this branch entirely once the legacy
	// pre-bundle disk-only sessions on the coordinator host have aged
	// out of relevance.
	Local bool
}

// Resolver looks up the worker that owns a given session.
type Resolver struct {
	store              *store.Store
	localAuditFallback bool
	// localHosts is the set of host strings (lowercased, no port) that
	// the resolver treats as "the coordinator itself" for the local
	// fall-back rule. Always includes the loopbacks; callers can append
	// the coordinator's external hostname via WithLocalHost.
	localHosts map[string]struct{}
}

// NewResolver constructs a Resolver. The store is used for both the
// session→worker lookup and the workers table read for the audit_url.
func NewResolver(st *store.Store) *Resolver {
	r := &Resolver{
		store:      st,
		localHosts: map[string]struct{}{},
	}
	for _, h := range []string{"127.0.0.1", "::1", "localhost", "0.0.0.0"} {
		r.localHosts[h] = struct{}{}
	}
	return r
}

// WithLocalAuditFallback enables the opt-in flag described in the Phase 2
// scope: when true, a session lookup that resolves to a worker whose
// audit_url points at this coordinator's own host short-circuits to the
// local audit handler instead of dialling itself in a loop.
func (r *Resolver) WithLocalAuditFallback(enabled bool) *Resolver {
	if r == nil {
		return nil
	}
	r.localAuditFallback = enabled
	return r
}

// WithLocalHost adds a hostname that should be treated as loopback when
// deciding whether to short-circuit to the local handler. Useful when the
// coordinator's hostname is reachable on the same box where workers
// register their audit_url. Empty values are ignored.
func (r *Resolver) WithLocalHost(host string) *Resolver {
	if r == nil {
		return nil
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return r
	}
	r.localHosts[host] = struct{}{}
	return r
}

// Resolve returns the worker that owns sessionID. The chain is:
//
//	session_id    → worker_id  via the session_routes index
//	worker_id     → audit_url  via the workers table
//	audit_url     → loopback?  → fall back to local handler
//
// session_routes is populated by the worker via AnnounceSession (and
// re-seeded on Register from RegisterRequest.OpenSessions). When no row
// exists the resolver returns ErrSessionNotRouted; the handler falls
// through to the coordinator-local handler so legacy `serve`-mode rows
// in the shared sessions table still resolve.
//
// A non-nil error is one of the package sentinels; callers translate to
// HTTP status. The result's Local flag is set when the caller should
// hand the request to the coordinator's existing local handler instead
// of issuing a proxy request.
func (r *Resolver) Resolve(sessionID string) (*Resolution, error) {
	if r == nil || r.store == nil {
		return nil, ErrSessionNotRouted
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, ErrSessionNotRouted
	}
	// session_id → worker_id via session_routes (the coord-side index
	// populated by worker AnnounceSession). Falls back to the legacy
	// sessions table read for `serve` mode (shared DB), where the
	// in-process worker has historically written rows directly.
	workerID := ""
	route, err := r.store.GetSessionRoute(sessionID)
	if err != nil {
		return nil, fmt.Errorf("sessionproxy: get session route: %w", err)
	}
	if route != nil {
		workerID = strings.TrimSpace(route.WorkerID)
	} else {
		rec, lerr := r.store.GetSession(sessionID)
		if lerr != nil {
			return nil, fmt.Errorf("sessionproxy: get session: %w", lerr)
		}
		if rec == nil {
			return nil, ErrSessionNotRouted
		}
		workerID = strings.TrimSpace(rec.WorkerID)
	}
	if workerID == "" {
		// Pre-bundle / orphan rows have no worker. Let the local handler
		// serve them — the data is on this host's sessions dir.
		return &Resolution{Local: true}, nil
	}
	worker, err := r.store.GetWorker(workerID)
	if err != nil {
		return nil, fmt.Errorf("sessionproxy: get worker: %w", err)
	}
	if worker == nil {
		// Worker row vanished (operator deleted, DB corruption). The
		// session row's local artefacts may still be reachable on the
		// coordinator host — fall back to local before giving up.
		return &Resolution{WorkerID: workerID, Local: true}, nil
	}
	auditURL := strings.TrimRight(strings.TrimSpace(worker.AuditURL), "/")
	if auditURL == "" {
		// Worker registered but never advertised an audit URL. Two real-
		// world causes both want the same outcome:
		//   1. `workbuddy serve` runs an in-process worker that doesn't
		//      pass --audit-listen, so audit_url is empty even though
		//      the data IS reachable via the shared coordinator DB.
		//   2. An operator rolls a new coordinator out before upgrading
		//      its workers; the old workers haven't learned to advertise
		//      audit_url yet but their session data still exists on the
		//      coordinator host's pre-Phase-2 disk store.
		// Falling through to the local handler covers both. The caller
		// (handler.serveLocal) returns 404 cleanly when the local store
		// has nothing either, so the user-visible failure mode is "no
		// session" instead of "503 worker has no audit_url".
		return &Resolution{WorkerID: workerID, Local: true}, nil
	}
	if r.isLocalURL(auditURL) {
		// audit_url points back at us. Avoid the dial-self loop.
		return &Resolution{WorkerID: workerID, AuditURL: auditURL, Local: true}, nil
	}
	return &Resolution{WorkerID: workerID, AuditURL: auditURL}, nil
}

// CandidateWorkers returns the workers eligible for a fan-out listing.
// When repo is non-empty the result is filtered by `workers.repos_json`
// (or the legacy `workers.repo` column) — this is delegated to
// Store.QueryWorkers, which already implements both. When repo is empty
// the full registry is returned.
func (r *Resolver) CandidateWorkers(repo string) ([]store.WorkerRecord, error) {
	if r == nil || r.store == nil {
		return nil, nil
	}
	rows, err := r.store.QueryWorkers(strings.TrimSpace(repo))
	if err != nil {
		return nil, fmt.Errorf("sessionproxy: query workers: %w", err)
	}
	return rows, nil
}

// isLocalURL reports whether u's host portion matches one of the
// coordinator's "self" hosts. Used to short-circuit local fall-back so
// the coordinator does not proxy to itself.
//
// The short-circuit is opt-in via WithLocalAuditFallback(true): in a
// split-host deployment, an audit_url that includes the worker's own
// loopback (127.0.0.1) would only happen if the worker bound on
// loopback AND advertised it literally — that's already a Phase-1
// misconfiguration, not a routing concern. We trust the registered
// audit_url and proxy unless the operator has explicitly told us "this
// is the same box, please serve locally".
//
// Phase 3 hook: once the local-fallback codepath is removed, this can
// be deleted along with localHosts and the WithLocalHost helper.
func (r *Resolver) isLocalURL(raw string) bool {
	if r == nil || !r.localAuditFallback {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return false
	}
	if _, ok := r.localHosts[host]; ok {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}
