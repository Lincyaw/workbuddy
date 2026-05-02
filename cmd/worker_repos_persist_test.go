package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadWorkerRepoBindingsFile_MissingReturnsNil(t *testing.T) {
	got, err := loadWorkerRepoBindingsFile(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("missing file: unexpected err: %v", err)
	}
	if got != nil {
		t.Fatalf("missing file: want nil, got %#v", got)
	}
}

func TestLoadWorkerRepoBindingsFile_EmptyPathReturnsNil(t *testing.T) {
	got, err := loadWorkerRepoBindingsFile("   ")
	if err != nil || got != nil {
		t.Fatalf("empty path: got %v / err %v; want nil/nil", got, err)
	}
}

func TestWriteAndLoadWorkerRepoBindingsFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "worker-repos.yaml")
	in := []workerRepoBinding{
		{Repo: "Lincyaw/zeta", Path: "/tmp/z"},
		{Repo: "Lincyaw/alpha", Path: "/tmp/a"},
	}
	if err := writeWorkerRepoBindingsFile(path, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("perm: got %v want 0600", mode)
	}
	got, err := loadWorkerRepoBindingsFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []workerRepoBinding{
		{Repo: "Lincyaw/alpha", Path: "/tmp/a"},
		{Repo: "Lincyaw/zeta", Path: "/tmp/z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch.\n got: %#v\nwant: %#v", got, want)
	}
}

func TestLoadWorkerRepoBindingsFile_RejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"missing repo": "schema_version: 1\nbindings:\n  - path: /tmp/x\n",
		"missing path": "schema_version: 1\nbindings:\n  - repo: x/y\n",
		"duplicate":    "schema_version: 1\nbindings:\n  - {repo: x/y, path: /a}\n  - {repo: x/y, path: /b}\n",
		"future schema": "schema_version: 9999\nbindings: []\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadWorkerRepoBindingsFile(path)
			if err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestMergeRepoBindings_FileWinsOnConflict(t *testing.T) {
	cli := []workerRepoBinding{
		{Repo: "owner/cli-only", Path: "/cli/only"},
		{Repo: "owner/both", Path: "/cli/both"},
	}
	file := []workerRepoBinding{
		{Repo: "owner/file-only", Path: "/file/only"},
		{Repo: "owner/both", Path: "/file/both"},
	}
	got := mergeRepoBindings(cli, file)
	want := []workerRepoBinding{
		{Repo: "owner/both", Path: "/file/both"},
		{Repo: "owner/cli-only", Path: "/cli/only"},
		{Repo: "owner/file-only", Path: "/file/only"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge mismatch.\n got: %#v\nwant: %#v", got, want)
	}
}

func TestMergeRepoBindings_DropsEmptyRepoEntries(t *testing.T) {
	got := mergeRepoBindings(
		[]workerRepoBinding{{Repo: "", Path: "/x"}, {Repo: "owner/a", Path: "/a"}},
		[]workerRepoBinding{{Repo: "", Path: "/y"}},
	)
	if len(got) != 1 || got[0].Repo != "owner/a" {
		t.Fatalf("expected single owner/a entry, got %#v", got)
	}
}

func TestDefaultWorkerReposFilePath_HonoursEnv(t *testing.T) {
	t.Setenv("WORKBUDDY_WORKER_REPOS_FILE", "/tmp/explicit.yaml")
	got, err := defaultWorkerReposFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/explicit.yaml" {
		t.Fatalf("env override: got %q", got)
	}
}

func TestDefaultWorkerReposFilePath_HonoursXDG(t *testing.T) {
	t.Setenv("WORKBUDDY_WORKER_REPOS_FILE", "")
	t.Setenv("XDG_CONFIG_HOME", "/custom/cfg")
	got, err := defaultWorkerReposFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "/custom/cfg/workbuddy/worker-repos.yaml") {
		t.Fatalf("xdg path: got %q", got)
	}
}

func TestWriteWorkerRepoBindingsFile_Atomic(t *testing.T) {
	// Verify the temp-file-then-rename pattern by snooping the parent
	// directory: after a successful write, no orphan .tmp files remain.
	dir := t.TempDir()
	path := filepath.Join(dir, "worker-repos.yaml")
	if err := writeWorkerRepoBindingsFile(path, []workerRepoBinding{{Repo: "x/y", Path: "/p"}}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("orphan temp file %q remained after atomic write", e.Name())
		}
	}
}
