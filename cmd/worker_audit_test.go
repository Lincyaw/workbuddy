package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
	workeraudit "github.com/Lincyaw/workbuddy/internal/worker/audit"
	"github.com/Lincyaw/workbuddy/internal/workerclient"
)

// TestStartWorkerAuditServer_DisabledShortCircuits verifies that
// "disabled" / empty-string opt-out leaves the audit server unstarted and
// the advertised URL empty so the register payload omits audit_url.
func TestStartWorkerAuditServer_DisabledShortCircuits(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, listen := range []string{"", "disabled", "DISABLED"} {
		opts := &workerOpts{auditListen: listen, token: "secret", sessionsDir: t.TempDir()}
		srv, url, err := startWorkerAuditServer(opts, st)
		if err != nil {
			t.Fatalf("listen=%q: unexpected error: %v", listen, err)
		}
		if srv != nil {
			t.Fatalf("listen=%q: expected nil server", listen)
		}
		if url != "" {
			t.Fatalf("listen=%q: expected empty URL, got %q", listen, url)
		}
	}
}

// TestStartWorkerAuditServer_FailsClosedWithoutToken verifies the
// audit listener refuses to start when WORKBUDDY_AUTH_TOKEN is unset.
func TestStartWorkerAuditServer_FailsClosedWithoutToken(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	opts := &workerOpts{auditListen: "127.0.0.1:0", token: "", sessionsDir: t.TempDir()}
	srv, _, err := startWorkerAuditServer(opts, st)
	if err == nil {
		t.Fatal("expected error when token is empty")
	}
	if srv != nil {
		_ = srv.Shutdown(t.Context())
		t.Fatal("expected nil server when token is empty")
	}
	if !strings.Contains(err.Error(), "WORKBUDDY_AUTH_TOKEN") {
		t.Fatalf("error = %v, want token-required mention", err)
	}
}

// TestWorkerAuditServer_RegisterAndServeRoundtrip wires together the
// audit server + register protocol: the worker starts the audit
// listener, registers against a fake coordinator (which captures the
// audit_url field), and a separate audit-aware HTTP client successfully
// reads /api/v1/sessions with the bearer token. Phase 1's full happy
// path.
func TestWorkerAuditServer_RegisterAndServeRoundtrip(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessionsDir := t.TempDir()

	const token = "secret-token-xyz"
	auditServer, err := workeraudit.Start(workeraudit.Config{
		Listen:      "127.0.0.1:0",
		Token:       token,
		Store:       st,
		SessionsDir: sessionsDir,
	})
	if err != nil {
		t.Fatalf("audit Start: %v", err)
	}
	t.Cleanup(func() { _ = auditServer.Shutdown(t.Context()) })
	advertised := workeraudit.AdvertisedURL(auditServer.Addr(), "", "")
	if advertised == "" {
		t.Fatal("AdvertisedURL returned empty")
	}

	// Fake coordinator that captures the register payload.
	var captured atomic.Value // workerclient.RegisterRequest
	fakeCoordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workers/register" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req workerclient.RegisterRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured.Store(req)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(fakeCoordinator.Close)

	client := workerclient.New(fakeCoordinator.URL, token, nil)
	if err := client.Register(t.Context(), workerclient.RegisterRequest{
		WorkerID: "w-1",
		Repo:     "owner/repo",
		Roles:    []string{"dev"},
		Hostname: "host",
		AuditURL: advertised,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, _ := captured.Load().(workerclient.RegisterRequest)
	if got.AuditURL != advertised {
		t.Fatalf("audit_url not propagated: got %q want %q", got.AuditURL, advertised)
	}

	// Hit the audit endpoint with the right bearer.
	req, _ := http.NewRequest(http.MethodGet, "http://"+auditServer.Addr()+"/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("audit GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit /api/v1/sessions status = %d", resp.StatusCode)
	}

	// Without the token: 401.
	reqBad, _ := http.NewRequest(http.MethodGet, "http://"+auditServer.Addr()+"/api/v1/sessions", nil)
	respBad, err := http.DefaultClient.Do(reqBad)
	if err != nil {
		t.Fatalf("unauth audit GET: %v", err)
	}
	defer func() { _ = respBad.Body.Close() }()
	if respBad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth audit /api/v1/sessions status = %d, want 401", respBad.StatusCode)
	}
}
