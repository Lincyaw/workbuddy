package security

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

const reloadDebounce = 200 * time.Millisecond

const (
	SourceNone = "none"
	SourceFlag = "flag"
	SourceEnv  = "env"
	SourceFile = "file"
)

type Config struct {
	TrustedAuthors []string `yaml:"trusted_authors"`
}

type Snapshot struct {
	TrustedAuthors []string
	Source         string

	index map[string]struct{}
}

type Options struct {
	FlagValue string
	FlagSet   bool
	EnvValue  string
	FilePath  string
}

type Runtime struct {
	filePath string
	value    atomic.Value // *Snapshot
}

func NewRuntime(opts Options) (*Runtime, bool, error) {
	snapshot, watchFile, err := loadInitial(opts)
	if err != nil {
		return nil, false, err
	}
	r := &Runtime{filePath: strings.TrimSpace(opts.FilePath)}
	r.value.Store(snapshot)
	return r, watchFile, nil
}

func (r *Runtime) Current() Snapshot {
	ptr := r.value.Load().(*Snapshot)
	return Snapshot{
		TrustedAuthors: append([]string(nil), ptr.TrustedAuthors...),
		Source:         ptr.Source,
		index:          copyIndex(ptr.index),
	}
}

func (r *Runtime) Allows(author string) bool {
	return r.Current().Allows(author)
}

func (s Snapshot) Allows(author string) bool {
	if len(s.index) == 0 {
		return true
	}
	_, ok := s.index[normalizeAuthor(author)]
	return ok
}

func (s Snapshot) IsRestricted() bool {
	return len(s.TrustedAuthors) > 0
}

func (s Snapshot) FormatAuthors() string {
	if len(s.TrustedAuthors) == 0 {
		return "[]"
	}
	return "[" + strings.Join(s.TrustedAuthors, ", ") + "]"
}

func (r *Runtime) StartFileWatcher(ctx context.Context) error {
	if strings.TrimSpace(r.filePath) == "" {
		return nil
	}
	dir := filepath.Dir(r.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("security: create watch dir %s: %w", dir, err)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("security: create watcher: %w", err)
	}
	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("security: watch %s: %w", dir, err)
	}

	go func() {
		defer func() { _ = watcher.Close() }()

		var (
			timer   *time.Timer
			timerCh <-chan time.Time
		)
		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("[security] watcher error: %v", err)
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if !isSecurityConfigEvent(r.filePath, event) {
					continue
				}
				if timer == nil {
					timer = time.NewTimer(reloadDebounce)
					timerCh = timer.C
					continue
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(reloadDebounce)
			case <-timerCh:
				timer = nil
				timerCh = nil
				r.reloadFromFile()
			}
		}
	}()
	return nil
}

func (r *Runtime) reloadFromFile() {
	prev := r.Current()
	next, err := loadFileSnapshot(r.filePath)
	if err != nil {
		log.Printf("[security] reload failed for %s: %v", r.filePath, err)
		return
	}
	r.value.Store(next)
	if next.IsRestricted() {
		log.Printf("[security] reloaded trusted_authors: %s (was: %s)", next.FormatAuthors(), prev.FormatAuthors())
		return
	}
	if prev.IsRestricted() {
		log.Printf("[security] warning: %s missing or empty; reverting trusted_authors to unrestricted (was: %s)", r.filePath, prev.FormatAuthors())
		return
	}
	log.Printf("[security] reloaded trusted_authors: unrestricted (was: %s)", prev.FormatAuthors())
}

func loadInitial(opts Options) (*Snapshot, bool, error) {
	if opts.FlagSet {
		return newSnapshot(parseAuthorList(opts.FlagValue), SourceFlag), false, nil
	}
	if strings.TrimSpace(opts.EnvValue) != "" {
		return newSnapshot(parseAuthorList(opts.EnvValue), SourceEnv), false, nil
	}
	snapshot, err := loadFileSnapshot(opts.FilePath)
	if err != nil {
		return nil, true, err
	}
	return snapshot, true, nil
}

func loadFileSnapshot(path string) (*Snapshot, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return newSnapshot(nil, SourceNone), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newSnapshot(nil, SourceNone), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return newSnapshot(cfg.TrustedAuthors, SourceFile), nil
}

func parseAuthorList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		key := normalizeAuthor(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func newSnapshot(authors []string, source string) *Snapshot {
	normalized := parseAuthorList(strings.Join(authors, ","))
	index := make(map[string]struct{}, len(normalized))
	for _, author := range normalized {
		index[normalizeAuthor(author)] = struct{}{}
	}
	return &Snapshot{
		TrustedAuthors: normalized,
		Source:         source,
		index:          index,
	}
}

func normalizeAuthor(author string) string {
	return strings.ToLower(strings.TrimSpace(author))
}

func copyIndex(src map[string]struct{}) map[string]struct{} {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]struct{}, len(src))
	for key := range src {
		dst[key] = struct{}{}
	}
	return dst
}

func isSecurityConfigEvent(filePath string, event fsnotify.Event) bool {
	if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
		return false
	}
	return filepath.Clean(event.Name) == filepath.Clean(filePath)
}
