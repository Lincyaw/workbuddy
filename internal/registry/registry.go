package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// Registry manages worker registration, heartbeat, and online/offline detection.
type Registry struct {
	store        *store.Store
	pollInterval time.Duration
}

// NewRegistry creates a Registry backed by the given store.
// pollInterval is used to calculate the staleness threshold (3 * pollInterval).
func NewRegistry(s *store.Store, pollInterval time.Duration) *Registry {
	return &Registry{
		store:        s,
		pollInterval: pollInterval,
	}
}

// Register adds a worker to the registry.
// roles is stored as a JSON array string in the DB.
func (r *Registry) Register(id, repo string, roles []string, hostname string) error {
	rolesJSON, err := json.Marshal(roles)
	if err != nil {
		return fmt.Errorf("registry: marshal roles: %w", err)
	}
	return r.store.InsertWorker(store.WorkerRecord{
		ID:       id,
		Repo:     repo,
		Roles:    string(rolesJSON),
		Hostname: hostname,
		Status:   "online",
	})
}

// Heartbeat updates the last_heartbeat timestamp for a worker.
func (r *Registry) Heartbeat(id string) error {
	return r.store.UpdateWorkerHeartbeat(id)
}

// MarkStaleOffline finds workers that are online but whose last_heartbeat
// is older than 3 * pollInterval, and marks them offline.
func (r *Registry) MarkStaleOffline() error {
	threshold := 3 * r.pollInterval
	db := r.store.DB()

	rows, err := db.Query(
		`SELECT id FROM workers WHERE status = 'online' AND last_heartbeat < datetime('now', ?)`,
		fmt.Sprintf("-%d seconds", int(threshold.Seconds())),
	)
	if err != nil {
		return fmt.Errorf("registry: query stale workers: %w", err)
	}
	defer rows.Close()

	var staleIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("registry: scan stale worker: %w", err)
		}
		staleIDs = append(staleIDs, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("registry: iterate stale workers: %w", err)
	}

	for _, id := range staleIDs {
		if err := r.store.UpdateWorkerStatus(id, "offline"); err != nil {
			return fmt.Errorf("registry: mark worker %q offline: %w", id, err)
		}
	}
	return nil
}

// FindWorkers returns online workers matching the given repo whose roles
// JSON array contains the requested role.
func (r *Registry) FindWorkers(repo, role string) ([]store.WorkerRecord, error) {
	// Get all online workers for the repo, then filter by role in Go.
	// This avoids complex JSON queries in SQLite while keeping things simple.
	workers, err := r.store.QueryWorkers(repo)
	if err != nil {
		return nil, fmt.Errorf("registry: find workers: %w", err)
	}

	var matched []store.WorkerRecord
	for _, w := range workers {
		if w.Status != "online" {
			continue
		}
		var roles []string
		if err := json.Unmarshal([]byte(w.Roles), &roles); err != nil {
			continue // skip workers with malformed roles
		}
		for _, r := range roles {
			if r == role {
				matched = append(matched, w)
				break
			}
		}
	}
	return matched, nil
}

// RegisterEmbedded registers an embedded (in-process) worker for v0.1.0.
// It generates an ID like "embedded-<hostname>" and returns it.
func (r *Registry) RegisterEmbedded(repo string, roles []string) (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	id := "embedded-" + hostname
	if err := r.Register(id, repo, roles, hostname); err != nil {
		return "", fmt.Errorf("registry: register embedded: %w", err)
	}
	return id, nil
}
