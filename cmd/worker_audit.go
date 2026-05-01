package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/store"
	workeraudit "github.com/Lincyaw/workbuddy/internal/worker/audit"
)

// startWorkerAuditServer starts the worker-side session audit HTTP server
// when --audit-listen is non-empty and not "disabled". Returns the server
// (or nil when disabled), the URL to advertise to the coordinator (or ""
// when disabled), and any startup error.
//
// Token policy: the audit listener reuses WORKBUDDY_AUTH_TOKEN. When the
// audit listener is enabled but no token is set, the worker fails fast
// rather than starting an unauthenticated audit server. Operators who do
// not want an audit listener must pass --audit-listen=disabled (or
// --audit-listen=) explicitly.
func startWorkerAuditServer(opts *workerOpts, st *store.Store) (*workeraudit.Server, string, error) {
	if opts == nil {
		return nil, "", nil
	}
	listen := strings.TrimSpace(opts.auditListen)
	if listen == "" || strings.EqualFold(listen, "disabled") {
		return nil, "", nil
	}
	token := strings.TrimSpace(opts.token)
	if token == "" {
		return nil, "", fmt.Errorf("worker audit: WORKBUDDY_AUTH_TOKEN (or --token-file) is required when --audit-listen is enabled; pass --audit-listen=disabled to opt out")
	}
	srv, err := workeraudit.Start(workeraudit.Config{
		Listen:      listen,
		Token:       token,
		Store:       st,
		SessionsDir: opts.sessionsDir,
	})
	if err != nil {
		return nil, "", fmt.Errorf("worker audit: %w", err)
	}
	hostname, _ := os.Hostname()
	advertised := workeraudit.AdvertisedURL(srv.Addr(), opts.auditPublicURL, hostname)
	return srv, advertised, nil
}
