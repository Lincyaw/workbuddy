// Package audit serves the worker-side, read-only session audit HTTP API.
//
// Phase 1 of the session-data ownership refactor (REQ-120; see
// docs/decisions/2026-05-01-session-data-ownership.md). The worker owns its
// own SQLite DB (.workbuddy/worker.db) and per-session artefacts directory;
// the coordinator-side audit handler reads from a different DB
// (.workbuddy/workbuddy.db) which never receives session rows in the
// 3-unit bundle topology. This server lets external tooling — and, in
// Phase 2, the coordinator itself — read session data straight from the
// worker that owns it.
//
// Endpoints (mounted on the audit listener):
//
//   - GET  /api/v1/sessions[?repo=&agent=&issue=&limit=&offset=]
//   - GET  /api/v1/sessions/{id}
//   - GET  /api/v1/sessions/{id}/events
//   - GET  /api/v1/sessions/{id}/stream      (SSE)
//   - GET  /health                            (no auth)
//
// All endpoints under /api/v1/ require an `Authorization: Bearer <token>`
// header where the token matches WORKBUDDY_AUTH_TOKEN. Missing/mismatched
// tokens get HTTP 401.
//
// The handler reuses internal/auditapi.Handler. Phase 3 of the
// session-data ownership refactor (REQ-122) deleted the disk-only
// synthesis fallback in auditapi.Handler entirely, so a missing row is a
// real 404 — no toggle needed. Events/stream paths reuse
// internal/webui.Handler unchanged.
package audit

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/auditapi"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/webui"
)

// Server is a worker-side HTTP audit server. Construct via Start; the zero
// value is unusable.
type Server struct {
	listener net.Listener
	server   *http.Server
	addr     string
}

// Config bundles the inputs needed to start the audit server.
type Config struct {
	// Listen is the bind address (e.g. "127.0.0.1:0", "0.0.0.0:8091").
	// Required.
	Listen string
	// Token is the bearer token clients must present. Required and
	// non-empty: the server refuses to start when Token is empty so an
	// operator cannot accidentally expose unauthenticated audit access.
	// Use the worker's existing WORKBUDDY_AUTH_TOKEN.
	Token string
	// Store is the worker's local store (.workbuddy/worker.db).
	Store *store.Store
	// SessionsDir is the absolute path to the directory containing
	// per-session artefacts (events-v1.jsonl, metadata.json, ...).
	SessionsDir string
}

// Start binds the listener and serves the audit API. The returned Server's
// Addr() reports the resolved listen address (useful when Listen is
// "127.0.0.1:0" and the kernel picks a port). Shutdown stops the server.
func Start(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Listen) == "" {
		return nil, fmt.Errorf("worker audit: Listen is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("worker audit: bearer token is required (set WORKBUDDY_AUTH_TOKEN or pass --audit-listen=disabled)")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("worker audit: store is required")
	}
	if strings.TrimSpace(cfg.SessionsDir) == "" {
		return nil, fmt.Errorf("worker audit: SessionsDir is required")
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("worker audit: listen %s: %w", cfg.Listen, err)
	}

	mux := buildMux(cfg)

	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()

	return &Server{
		listener: ln,
		server:   srv,
		addr:     ln.Addr().String(),
	}, nil
}

// buildMux assembles the audit HTTP routes. Exposed for tests.
func buildMux(cfg Config) http.Handler {
	dashboard := auditapi.NewHandler(cfg.Store)
	dashboard.SetSessionsDir(cfg.SessionsDir)

	events := webui.NewHandler(cfg.Store)
	events.SetSessionsDir(cfg.SessionsDir)
	dashboard.SetSessionEventsHandler(events.HandleAPISessionEvents)
	dashboard.SetSessionStreamHandler(events.HandleAPISessionStream)

	apiMux := http.NewServeMux()
	// Only the session list/detail/events/stream subset of the dashboard
	// API is exposed: the worker DB does not carry coordinator-only state
	// (issue caches, transition counts, workers, alerts, ...) so wiring
	// the rest would just return empty/garbage. Phase 2 may extend this.
	dashboard.RegisterAPISessions(apiMux)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/api/v1/", bearerAuth(cfg.Token, apiMux))
	return mux
}

// Addr returns the resolved listen address (host:port) of the server.
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

// Shutdown stops the HTTP server, waiting up to ctx's deadline for
// in-flight requests to drain.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// AdvertisedURL computes the URL workers should advertise to the
// coordinator in their RegisterRequest.AuditURL field. publicURL takes
// precedence; otherwise we build "http://<host>:<port>" from the resolved
// listener address. When the bind host is the wildcard 0.0.0.0/:: (i.e.
// the listener was opened on all interfaces), substitute the worker
// hostname so the URL is reachable from the coordinator host.
func AdvertisedURL(addr, publicURL, hostname string) string {
	if pu := strings.TrimRight(strings.TrimSpace(publicURL), "/"); pu != "" {
		return pu
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return ""
	}
	if isWildcardHost(host) {
		host = strings.TrimSpace(hostname)
		if host == "" {
			host = "127.0.0.1"
		}
	}
	return "http://" + net.JoinHostPort(host, port)
}

func isWildcardHost(host string) bool {
	switch strings.TrimSpace(host) {
	case "0.0.0.0", "::", "[::]", "":
		return true
	}
	return false
}
