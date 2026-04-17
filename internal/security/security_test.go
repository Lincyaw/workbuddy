package security

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewRuntimePrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".workbuddy", "security.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("trusted_authors:\n  - file-user\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		opts      Options
		want      []string
		wantSrc   string
		wantWatch bool
	}{
		{
			name: "flag overrides env and file",
			opts: Options{
				FlagValue: "flag-user,dependabot[bot]",
				FlagSet:   true,
				EnvValue:  "env-user",
				FilePath:  path,
			},
			want:      []string{"flag-user", "dependabot[bot]"},
			wantSrc:   SourceFlag,
			wantWatch: false,
		},
		{
			name: "env overrides file",
			opts: Options{
				EnvValue: "env-user",
				FilePath: path,
			},
			want:      []string{"env-user"},
			wantSrc:   SourceEnv,
			wantWatch: false,
		},
		{
			name: "file used when no flag or env",
			opts: Options{
				FilePath: path,
			},
			want:      []string{"file-user"},
			wantSrc:   SourceFile,
			wantWatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, watch, err := NewRuntime(tt.opts)
			if err != nil {
				t.Fatalf("NewRuntime: %v", err)
			}
			current := runtime.Current()
			if got, want := current.Source, tt.wantSrc; got != want {
				t.Fatalf("source = %q, want %q", got, want)
			}
			if got, want := watch, tt.wantWatch; got != want {
				t.Fatalf("watch = %v, want %v", got, want)
			}
			if len(current.TrustedAuthors) != len(tt.want) {
				t.Fatalf("trusted_authors = %v, want %v", current.TrustedAuthors, tt.want)
			}
			for i := range tt.want {
				if current.TrustedAuthors[i] != tt.want[i] {
					t.Fatalf("trusted_authors = %v, want %v", current.TrustedAuthors, tt.want)
				}
			}
		})
	}
}

func TestAllowsIsCaseInsensitive(t *testing.T) {
	runtime, _, err := NewRuntime(Options{FlagValue: "Alice,dependabot[bot]", FlagSet: true})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if !runtime.Allows("alice") {
		t.Fatal("expected lowercase alice to be allowed")
	}
	if !runtime.Allows("ALICE") {
		t.Fatal("expected uppercase ALICE to be allowed")
	}
	if !runtime.Allows("Dependabot[Bot]") {
		t.Fatal("expected mixed-case bot login to be allowed")
	}
	if runtime.Allows("mallory") {
		t.Fatal("expected mallory to be denied")
	}
}

func TestFileWatcherReloadsAndKeepsOldConfigOnParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".workbuddy", "security.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("trusted_authors:\n  - alice\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runtime, watch, err := NewRuntime(Options{FilePath: path})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if !watch {
		t.Fatal("expected file watch to be enabled")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runtime.StartFileWatcher(ctx); err != nil {
		t.Fatalf("StartFileWatcher: %v", err)
	}

	if err := os.WriteFile(path, []byte("trusted_authors:\n  - bob\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForAuthors(t, runtime, []string{"bob"})

	if err := os.WriteFile(path, []byte("trusted_authors: [alice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	waitForAuthors(t, runtime, []string{"bob"})

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitForAuthors(t, runtime, nil)
}

func waitForAuthors(t *testing.T, runtime *Runtime, want []string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got := runtime.Current().TrustedAuthors
		if equalAuthors(got, want) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("trusted_authors = %v, want %v", runtime.Current().TrustedAuthors, want)
}

func equalAuthors(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
