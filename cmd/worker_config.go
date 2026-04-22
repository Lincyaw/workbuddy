package cmd

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
)

type workerRepoConfigSummary struct {
	Repo      string   `json:"repo"`
	ConfigDir string   `json:"config_dir"`
	Warnings  []string `json:"warnings,omitempty"`
}

type workerConfigReloadSummary struct {
	Repos      []workerRepoConfigSummary `json:"repos"`
	ReloadedAt time.Time                 `json:"reloaded_at"`
}

type workerRepoConfigStore struct {
	mu        sync.RWMutex
	configDir string
	configs   map[string]*config.FullConfig
}

func newWorkerRepoConfigStore(configDir string) *workerRepoConfigStore {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		configDir = ".github/workbuddy"
	}
	return &workerRepoConfigStore{
		configDir: configDir,
		configs:   make(map[string]*config.FullConfig),
	}
}

func (s *workerRepoConfigStore) get(repo string) (*config.FullConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg, ok := s.configs[repo]
	return cfg, ok
}

func (s *workerRepoConfigStore) list() []*config.FullConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*config.FullConfig, 0, len(s.configs))
	for _, cfg := range s.configs {
		out = append(out, cfg)
	}
	return out
}

func (s *workerRepoConfigStore) reload(bindings []workerRepoBinding) (*workerConfigReloadSummary, error) {
	next := make(map[string]*config.FullConfig, len(bindings))
	summary := &workerConfigReloadSummary{
		Repos:      make([]workerRepoConfigSummary, 0, len(bindings)),
		ReloadedAt: time.Now().UTC(),
	}

	for _, binding := range bindings {
		cfgDir := resolveWorkerConfigDir(binding.Path, s.configDir)
		cfg, warnings, err := config.LoadConfig(cfgDir)
		if err != nil {
			return nil, fmt.Errorf("worker: repo %s config load %q: %w", binding.Repo, cfgDir, err)
		}
		next[binding.Repo] = cfg

		repoSummary := workerRepoConfigSummary{
			Repo:      binding.Repo,
			ConfigDir: cfgDir,
		}
		for _, warning := range warnings {
			message := warning.String()
			repoSummary.Warnings = append(repoSummary.Warnings, message)
			log.Printf("[worker] warning: repo=%s %s", binding.Repo, message)
		}
		summary.Repos = append(summary.Repos, repoSummary)
	}

	s.mu.Lock()
	s.configs = next
	s.mu.Unlock()
	return summary, nil
}

func resolveWorkerConfigDir(repoPath, configDir string) string {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		configDir = ".github/workbuddy"
	}
	if filepath.IsAbs(configDir) {
		return filepath.Clean(configDir)
	}
	return filepath.Join(repoPath, configDir)
}
