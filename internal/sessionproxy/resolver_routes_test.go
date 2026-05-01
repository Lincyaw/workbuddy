package sessionproxy

import (
	"path/filepath"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestResolverPrefersSessionRoutes is the regression test for the
// detail-path 404 the user hit on a coordinator running REQ-122 Phase 3:
// the legacy sessions table was dropped on coord, so GetSession
// returned nil for everything, the resolver gave up, and detail reads
// 404'd instead of being proxied to the owning worker. After the
// follow-up, the resolver consults session_routes first; with a row
// installed by AnnounceSession the proxy now finds the right worker
// even though the coord's `sessions` table no longer exists.
func TestResolverPrefersSessionRoutes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "coord.db")
	st, err := store.NewCoordinatorStore(dbPath)
	if err != nil {
		t.Fatalf("NewCoordinatorStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InsertWorker(store.WorkerRecord{
		ID:       "worker-a",
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://worker-a.example:8091",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	if err := st.UpsertSessionRoute(store.SessionRoute{
		SessionID: "sess-1",
		WorkerID:  "worker-a",
		Repo:      "org/repo",
		IssueNum:  42,
	}); err != nil {
		t.Fatalf("UpsertSessionRoute: %v", err)
	}

	res, err := NewResolver(st).Resolve("sess-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res == nil || res.Local {
		t.Fatalf("Resolve = %+v, want remote routing", res)
	}
	if res.WorkerID != "worker-a" {
		t.Fatalf("worker_id = %q, want worker-a", res.WorkerID)
	}
	if res.AuditURL != "http://worker-a.example:8091" {
		t.Fatalf("audit_url = %q, want http://worker-a.example:8091", res.AuditURL)
	}
}

// TestResolverNoRouteNoSession — the resolver returns ErrSessionNotRouted
// when neither session_routes nor sessions has the id, so the handler
// falls through to the local handler (which 404s cleanly on a coord
// store that has no sessions table at all).
func TestResolverNoRouteNoSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "coord.db")
	st, err := store.NewCoordinatorStore(dbPath)
	if err != nil {
		t.Fatalf("NewCoordinatorStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	_, err = NewResolver(st).Resolve("missing")
	if err != ErrSessionNotRouted {
		t.Fatalf("Resolve(missing) = %v, want ErrSessionNotRouted", err)
	}
}

// TestResolverFallsBackToLegacySessions — `serve` mode uses
// store.NewStore (shared DB, sessions table intact) and historically
// inserts session rows directly. Until the in-process worker also
// announces, the resolver must keep the legacy lookup functional, so
// existing serve-mode deployments don't break under the new code.
func TestResolverFallsBackToLegacySessions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "serve.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InsertWorker(store.WorkerRecord{
		ID:       "worker-a",
		Repo:     "org/repo",
		Roles:    `["dev"]`,
		Hostname: "host",
		AuditURL: "http://worker-a.example:8091",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	if _, err := st.CreateSession(store.SessionRecord{
		SessionID: "sess-legacy",
		Repo:      "org/repo",
		IssueNum:  1,
		AgentName: "dev-agent",
		WorkerID:  "worker-a",
		Status:    "running",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Note: no UpsertSessionRoute here. The resolver should still find
	// the worker via the legacy sessions table.

	res, err := NewResolver(st).Resolve("sess-legacy")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res == nil || res.WorkerID != "worker-a" || res.AuditURL == "" {
		t.Fatalf("Resolve = %+v, want worker-a with audit_url", res)
	}
}
