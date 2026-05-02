package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// defaultWorkerReposFilePath returns the canonical persistent location for
// runtime-mutated worker repo bindings. Honours $XDG_CONFIG_HOME, falls
// back to ~/.config. The file is mutated by `worker repos add/remove` and
// loaded at worker startup, so dynamic bindings survive restarts (which
// the static --repos flag alone does not provide).
func defaultWorkerReposFilePath() (string, error) {
	if envPath := strings.TrimSpace(os.Getenv("WORKBUDDY_WORKER_REPOS_FILE")); envPath != "" {
		return envPath, nil
	}
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("worker repos file: resolve home: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "workbuddy", "worker-repos.yaml"), nil
}

// workerReposFileSchema is the on-disk YAML shape. The top-level wrapper
// gives us a place to add metadata later (schema version, last-modified
// timestamp) without breaking older callers.
type workerReposFileSchema struct {
	SchemaVersion int                 `yaml:"schema_version,omitempty"`
	Bindings      []workerRepoBinding `yaml:"bindings"`
}

const workerReposFileSchemaVersion = 1

// loadWorkerRepoBindingsFile reads bindings from path. A missing file is
// treated as the empty set (nil, nil) so first-time startup is silent.
// Validation rejects empty repo / path entries because they would silently
// poison the in-memory store.
func loadWorkerRepoBindingsFile(path string) ([]workerRepoBinding, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("worker repos file %s: read: %w", path, err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var schema workerReposFileSchema
	if err := yaml.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("worker repos file %s: parse: %w", path, err)
	}
	if schema.SchemaVersion < 0 || schema.SchemaVersion > workerReposFileSchemaVersion {
		return nil, fmt.Errorf("worker repos file %s: unsupported schema_version %d (max %d)", path, schema.SchemaVersion, workerReposFileSchemaVersion)
	}
	out := make([]workerRepoBinding, 0, len(schema.Bindings))
	seen := make(map[string]struct{}, len(schema.Bindings))
	for i, b := range schema.Bindings {
		b.Repo = strings.TrimSpace(b.Repo)
		b.Path = strings.TrimSpace(b.Path)
		if b.Repo == "" || b.Path == "" {
			return nil, fmt.Errorf("worker repos file %s: bindings[%d]: repo and path are required", path, i)
		}
		if _, dup := seen[b.Repo]; dup {
			return nil, fmt.Errorf("worker repos file %s: duplicate repo %s", path, b.Repo)
		}
		seen[b.Repo] = struct{}{}
		out = append(out, b)
	}
	return out, nil
}

// writeWorkerRepoBindingsFile serializes bindings to YAML and writes them
// atomically (delegated to writeBytesAtomic, which handles temp-file +
// rename + cleanup).
func writeWorkerRepoBindingsFile(path string, bindings []workerRepoBinding) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("worker repos file: path is required")
	}
	sorted := append([]workerRepoBinding(nil), bindings...)
	slices.SortFunc(sorted, func(a, b workerRepoBinding) int {
		return strings.Compare(a.Repo, b.Repo)
	})
	data, err := yaml.Marshal(&workerReposFileSchema{
		SchemaVersion: workerReposFileSchemaVersion,
		Bindings:      sorted,
	})
	if err != nil {
		return fmt.Errorf("worker repos file: marshal: %w", err)
	}
	if err := writeBytesAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("worker repos file: %w", err)
	}
	return nil
}

// mergeRepoBindings combines CLI-supplied bindings with file-supplied
// bindings. The file wins on conflict because it is the authoritative
// record of operator-issued `worker repos add/remove` mutations — losing
// it across a worker restart was the bug the persistence layer is meant
// to fix. Result is deterministic order (sorted by repo).
func mergeRepoBindings(cli, file []workerRepoBinding) []workerRepoBinding {
	merged := make(map[string]string, len(cli)+len(file))
	for _, b := range cli {
		if b.Repo != "" {
			merged[b.Repo] = b.Path
		}
	}
	for _, b := range file {
		if b.Repo != "" {
			merged[b.Repo] = b.Path
		}
	}
	out := make([]workerRepoBinding, 0, len(merged))
	for repo, path := range merged {
		out = append(out, workerRepoBinding{Repo: repo, Path: path})
	}
	slices.SortFunc(out, func(a, b workerRepoBinding) int {
		return strings.Compare(a.Repo, b.Repo)
	})
	return out
}
