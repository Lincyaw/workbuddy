package registry

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func newTestRegistry(t *testing.T, pollInterval time.Duration) *Registry {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewRegistry(s, pollInterval)
}

func TestRegister(t *testing.T) {
	reg := newTestRegistry(t, 10*time.Second)

	err := reg.Register("w1", "owner/repo", []string{"dev", "test"}, "host1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Verify worker is stored and online.
	workers, err := reg.FindWorkers("owner/repo", "dev")
	if err != nil {
		t.Fatalf("find workers: %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(workers))
	}
	w := workers[0]
	if w.ID != "w1" {
		t.Errorf("expected id w1, got %s", w.ID)
	}
	if w.Hostname != "host1" {
		t.Errorf("expected hostname host1, got %s", w.Hostname)
	}
	if w.Status != "online" {
		t.Errorf("expected status online, got %s", w.Status)
	}

	// Roles should be a JSON array.
	var roles []string
	if err := json.Unmarshal([]byte(w.Roles), &roles); err != nil {
		t.Fatalf("unmarshal roles: %v", err)
	}
	if len(roles) != 2 || roles[0] != "dev" || roles[1] != "test" {
		t.Errorf("expected roles [dev test], got %v", roles)
	}
}

func TestHeartbeat(t *testing.T) {
	reg := newTestRegistry(t, 10*time.Second)

	err := reg.Register("w1", "owner/repo", []string{"dev"}, "host1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Heartbeat should succeed.
	if err := reg.Heartbeat("w1"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	// Heartbeat for non-existent worker should fail.
	if err := reg.Heartbeat("nonexistent"); err == nil {
		t.Error("expected error for non-existent worker heartbeat")
	}
}

func TestMarkStaleOffline(t *testing.T) {
	// Use a very short poll interval so 3*pollInterval is tiny.
	reg := newTestRegistry(t, 1*time.Millisecond)

	err := reg.Register("w1", "owner/repo", []string{"dev"}, "host1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Manually set last_heartbeat to the past so the worker appears stale.
	db := reg.store.DB()
	_, err = db.Exec(
		`UPDATE workers SET last_heartbeat = datetime('now', '-10 seconds') WHERE id = 'w1'`,
	)
	if err != nil {
		t.Fatalf("set old heartbeat: %v", err)
	}

	// Mark stale offline.
	if err := reg.MarkStaleOffline(); err != nil {
		t.Fatalf("mark stale offline: %v", err)
	}

	// Worker should now be offline, so FindWorkers returns empty.
	workers, err := reg.FindWorkers("owner/repo", "dev")
	if err != nil {
		t.Fatalf("find workers: %v", err)
	}
	if len(workers) != 0 {
		t.Errorf("expected 0 online workers, got %d", len(workers))
	}
}

func TestFindWorkersByRepoAndRole(t *testing.T) {
	reg := newTestRegistry(t, 10*time.Second)

	// Register workers in different repos with different roles.
	if err := reg.Register("w1", "owner/repo-a", []string{"dev", "test"}, "host1"); err != nil {
		t.Fatalf("register w1: %v", err)
	}
	if err := reg.Register("w2", "owner/repo-a", []string{"review"}, "host2"); err != nil {
		t.Fatalf("register w2: %v", err)
	}
	if err := reg.Register("w3", "owner/repo-b", []string{"dev"}, "host3"); err != nil {
		t.Fatalf("register w3: %v", err)
	}

	// Query repo-a + dev → only w1.
	workers, err := reg.FindWorkers("owner/repo-a", "dev")
	if err != nil {
		t.Fatalf("find workers: %v", err)
	}
	if len(workers) != 1 || workers[0].ID != "w1" {
		t.Errorf("expected [w1], got %v", workerIDs(workers))
	}

	// Query repo-a + review → only w2.
	workers, err = reg.FindWorkers("owner/repo-a", "review")
	if err != nil {
		t.Fatalf("find workers: %v", err)
	}
	if len(workers) != 1 || workers[0].ID != "w2" {
		t.Errorf("expected [w2], got %v", workerIDs(workers))
	}

	// Query repo-b + dev → only w3.
	workers, err = reg.FindWorkers("owner/repo-b", "dev")
	if err != nil {
		t.Fatalf("find workers: %v", err)
	}
	if len(workers) != 1 || workers[0].ID != "w3" {
		t.Errorf("expected [w3], got %v", workerIDs(workers))
	}

	// Query repo-a + nonexistent role → empty.
	workers, err = reg.FindWorkers("owner/repo-a", "deploy")
	if err != nil {
		t.Fatalf("find workers: %v", err)
	}
	if len(workers) != 0 {
		t.Errorf("expected empty, got %v", workerIDs(workers))
	}

	// Query nonexistent repo → empty.
	workers, err = reg.FindWorkers("owner/repo-c", "dev")
	if err != nil {
		t.Fatalf("find workers: %v", err)
	}
	if len(workers) != 0 {
		t.Errorf("expected empty, got %v", workerIDs(workers))
	}
}

func TestRegisterEmbedded(t *testing.T) {
	reg := newTestRegistry(t, 10*time.Second)

	id, err := reg.RegisterEmbedded("owner/repo", []string{"dev", "test"})
	if err != nil {
		t.Fatalf("register embedded: %v", err)
	}

	// ID should start with "embedded-".
	if len(id) < 10 || id[:9] != "embedded-" {
		t.Errorf("expected id starting with 'embedded-', got %s", id)
	}

	// Should be findable.
	workers, err := reg.FindWorkers("owner/repo", "dev")
	if err != nil {
		t.Fatalf("find workers: %v", err)
	}
	if len(workers) != 1 || workers[0].ID != id {
		t.Errorf("expected embedded worker, got %v", workerIDs(workers))
	}
}

func workerIDs(ws []store.WorkerRecord) []string {
	ids := make([]string, len(ws))
	for i, w := range ws {
		ids[i] = w.ID
	}
	return ids
}
