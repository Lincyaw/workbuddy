package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolveWorkerRepoBindings(t *testing.T) {
	defaultPath := t.TempDir()

	bindings, err := resolveWorkerRepoBindings(&workerOpts{reposCSV: "owner/a=" + defaultPath + ",owner/b=" + defaultPath}, "", defaultPath)
	if err != nil {
		t.Fatalf("resolve --repos: %v", err)
	}
	if len(bindings) != 2 || bindings[0].Repo != "owner/a" || bindings[1].Repo != "owner/b" {
		t.Fatalf("unexpected bindings: %+v", bindings)
	}

	bindings, err = resolveWorkerRepoBindings(&workerOpts{repo: "owner/c"}, "", defaultPath)
	if err != nil {
		t.Fatalf("resolve --repo: %v", err)
	}
	if len(bindings) != 1 || bindings[0].Path != defaultPath {
		t.Fatalf("unexpected legacy binding: %+v", bindings)
	}

	bindings, err = resolveWorkerRepoBindings(&workerOpts{}, "owner/d", defaultPath)
	if err != nil {
		t.Fatalf("resolve config repo: %v", err)
	}
	if len(bindings) != 1 || bindings[0].Repo != "owner/d" {
		t.Fatalf("unexpected config binding: %+v", bindings)
	}
}

func TestWorkerRepoBindingStoreOperations(t *testing.T) {
	store := newWorkerRepoBindingStore([]workerRepoBinding{{Repo: "owner/b", Path: "/b"}, {Repo: "owner/a", Path: "/a"}})

	if got, ok := store.get("owner/a"); !ok || got != "/a" {
		t.Fatalf("get owner/a = %q, %v", got, ok)
	}
	store.set("owner/c", "/c")
	store.delete("owner/b")

	got := store.list()
	want := []workerRepoBinding{
		{Repo: "owner/a", Path: "/a"},
		{Repo: "owner/c", Path: "/c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %+v, want %+v", got, want)
	}
}

func TestWorkerMgmtServerAddListRemoveAndCleanup(t *testing.T) {
	controlDir := t.TempDir()
	repoAPath := t.TempDir()
	repoBPath := t.TempDir()
	addrFile := workerAddrFile(controlDir)
	bindings := newWorkerRepoBindingStore([]workerRepoBinding{{Repo: "owner/a", Path: repoAPath}})

	changeCh := make(chan []string, 2)
	server, err := startWorkerMgmtServer(
		"127.0.0.1:0",
		addrFile,
		bindings,
		func(_ context.Context, repos []string) error {
			changeCh <- append([]string(nil), repos...)
			return nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("startWorkerMgmtServer: %v", err)
	}

	client, err := workerMgmtClientFromControlDir(controlDir)
	if err != nil {
		t.Fatalf("workerMgmtClientFromControlDir: %v", err)
	}

	listed, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Repo != "owner/a" || listed[0].Path != repoAPath {
		t.Fatalf("unexpected initial bindings: %+v", listed)
	}

	if err := client.Add(context.Background(), workerRepoBinding{Repo: "owner/b", Path: repoBPath}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := <-changeCh; !reflect.DeepEqual(got, []string{"owner/a", "owner/b"}) {
		t.Fatalf("change repos = %v", got)
	}

	if err := client.Remove(context.Background(), "owner/a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := <-changeCh; !reflect.DeepEqual(got, []string{"owner/b"}) {
		t.Fatalf("change repos after delete = %v", got)
	}

	if err := server.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(addrFile); !os.IsNotExist(err) {
		t.Fatalf("expected addr file cleanup, stat err = %v", err)
	}
}

func TestWorkerReposCommands(t *testing.T) {
	controlDir := t.TempDir()
	t.Chdir(controlDir)
	if err := os.MkdirAll(filepath.Join(controlDir, ".workbuddy"), 0755); err != nil {
		t.Fatal(err)
	}
	repoPath := t.TempDir()

	var gotPost workerRepoBinding
	var gotDelete string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/mgmt/repos":
			if err := json.NewDecoder(r.Body).Decode(&gotPost); err != nil {
				t.Fatalf("decode post: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/mgmt/repos":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"repo":"owner/a","path":"` + repoPath + `"}]`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/mgmt/repos/"):
			gotDelete = strings.TrimPrefix(r.URL.Path, "/mgmt/repos/")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	if err := os.WriteFile(workerAddrFile(controlDir), []byte(server.URL+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	workerReposAddCmd.SetContext(context.Background())
	if err := workerReposAddCmd.RunE(workerReposAddCmd, []string{"owner/a=" + repoPath}); err != nil {
		t.Fatalf("worker repos add: %v", err)
	}
	if gotPost.Repo != "owner/a" || gotPost.Path != repoPath {
		t.Fatalf("unexpected add payload: %+v", gotPost)
	}

	var out bytes.Buffer
	workerReposListCmd.SetContext(context.Background())
	workerReposListCmd.SetOut(&out)
	if err := workerReposListCmd.RunE(workerReposListCmd, nil); err != nil {
		t.Fatalf("worker repos list: %v", err)
	}
	if !strings.Contains(out.String(), "owner/a") || !strings.Contains(out.String(), repoPath) {
		t.Fatalf("unexpected list output: %q", out.String())
	}

	workerReposRemoveCmd.SetContext(context.Background())
	if err := workerReposRemoveCmd.RunE(workerReposRemoveCmd, []string{"owner/a"}); err != nil {
		t.Fatalf("worker repos remove: %v", err)
	}
	if gotDelete != "owner/a" {
		t.Fatalf("unexpected delete path: %q", gotDelete)
	}
}

func TestWorkerReposList_JSON(t *testing.T) {
	controlDir := t.TempDir()
	t.Chdir(controlDir)
	if err := os.MkdirAll(filepath.Join(controlDir, ".workbuddy"), 0o755); err != nil {
		t.Fatal(err)
	}
	repoPath := t.TempDir()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/mgmt/repos" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"repo":"owner/a","path":"` + repoPath + `"}]`))
	}))
	defer server.Close()

	if err := os.WriteFile(workerAddrFile(controlDir), []byte(server.URL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	workerReposListCmd.SetContext(context.Background())
	workerReposListCmd.SetOut(&out)
	if err := workerReposListCmd.Flags().Set("format", outputFormatJSON); err != nil {
		t.Fatalf("Set format: %v", err)
	}
	t.Cleanup(func() {
		_ = workerReposListCmd.Flags().Set("format", outputFormatText)
	})
	if err := workerReposListCmd.RunE(workerReposListCmd, nil); err != nil {
		t.Fatalf("worker repos list: %v", err)
	}

	var bindings []workerRepoBinding
	if err := json.Unmarshal(out.Bytes(), &bindings); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(bindings) != 1 || bindings[0].Repo != "owner/a" || bindings[0].Path != repoPath {
		t.Fatalf("unexpected bindings: %+v", bindings)
	}
}

func TestWorkerMgmtServerRejectsInvalidBinding(t *testing.T) {
	controlDir := t.TempDir()
	server, err := startWorkerMgmtServer("127.0.0.1:0", workerAddrFile(controlDir), newWorkerRepoBindingStore(nil), func(_ context.Context, repos []string) error {
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("startWorkerMgmtServer: %v", err)
	}
	defer func() {
		_ = server.Close(context.Background())
	}()

	client, err := workerMgmtClientFromControlDir(controlDir)
	if err != nil {
		t.Fatalf("workerMgmtClientFromControlDir: %v", err)
	}
	err = client.Add(context.Background(), workerRepoBinding{Repo: "owner/a", Path: filepath.Join(controlDir, "missing")})
	if err == nil || !strings.Contains(err.Error(), "stat path") {
		t.Fatalf("expected invalid path error, got %v", err)
	}
}

func TestWorkerMgmtClientFromControlDirMissingFileShowsSuggestedFix(t *testing.T) {
	controlDir := t.TempDir()

	_, err := workerMgmtClientFromControlDir(controlDir)
	if err == nil {
		t.Fatal("expected missing worker control file to fail")
	}
	addrPath := workerAddrFile(controlDir)
	if got, want := err.Error(), `worker repos: worker control file "`+addrPath+`" was not found. Start `+"`workbuddy worker`"+` in this repo before managing repo bindings.`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("error leaked raw syscall text: %v", err)
	}
}

func TestWorkerMgmtClientTimesOutWithReadableError(t *testing.T) {
	client := newWorkerMgmtClient("http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := client.Remove(ctx, "owner/a")
	if err == nil {
		t.Fatal("expected connection error")
	}
}

func TestValidateWorkerMgmtAddrRejectsNonLoopback(t *testing.T) {
	if err := validateWorkerMgmtAddr("0.0.0.0:0"); err == nil {
		t.Fatal("expected non-loopback addr rejection")
	}
	if err := validateWorkerMgmtAddr("127.0.0.1:0"); err != nil {
		t.Fatalf("loopback addr rejected: %v", err)
	}
}

func TestWorkerRepoConfigStoreReloadsPerRepoAndLogsWarnings(t *testing.T) {
	repoAPath := setupWorkerRepoConfig(t, "owner/a", `echo repo-a`, "status:developing")
	repoBPath := setupWorkerRepoConfig(t, "owner/b", `echo repo-b`, "status:missing")
	store := newWorkerRepoConfigStore(".github/workbuddy")

	var logs bytes.Buffer
	prevWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prevWriter)

	summary, err := store.reload([]workerRepoBinding{
		{Repo: "owner/a", Path: repoAPath},
		{Repo: "owner/b", Path: repoBPath},
	})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(summary.Repos) != 2 {
		t.Fatalf("summary repos = %d, want 2", len(summary.Repos))
	}
	cfgB, ok := store.get("owner/b")
	if !ok || cfgB.Agents["dev-agent"].Command != "echo repo-b" {
		t.Fatalf("repo B config not loaded correctly: ok=%v cfg=%+v", ok, cfgB)
	}
	if !strings.Contains(logs.String(), "repo=owner/b") {
		t.Fatalf("warning log missing repo slug: %q", logs.String())
	}

	setupWorkerRepoConfigAtPath(t, repoBPath, "owner/b", `echo repo-b-reloaded`, "status:missing")
	if _, err := store.reload([]workerRepoBinding{
		{Repo: "owner/a", Path: repoAPath},
		{Repo: "owner/b", Path: repoBPath},
	}); err != nil {
		t.Fatalf("reload second pass: %v", err)
	}
	cfgB, ok = store.get("owner/b")
	if !ok || cfgB.Agents["dev-agent"].Command != "echo repo-b-reloaded" {
		t.Fatalf("repo B config after reload = %+v, want reloaded command", cfgB)
	}
}

func TestWorkerMgmtServerConfigReloadEndpointReloadsAllRepoConfigs(t *testing.T) {
	controlDir := t.TempDir()
	repoAPath := setupWorkerRepoConfig(t, "owner/a", `echo repo-a`, "")
	repoBPath := setupWorkerRepoConfig(t, "owner/b", `echo repo-b`, "")
	bindings := newWorkerRepoBindingStore([]workerRepoBinding{
		{Repo: "owner/a", Path: repoAPath},
		{Repo: "owner/b", Path: repoBPath},
	})
	store := newWorkerRepoConfigStore(".github/workbuddy")
	if _, err := store.reload(bindings.list()); err != nil {
		t.Fatalf("initial reload: %v", err)
	}

	server, err := startWorkerMgmtServer(
		"127.0.0.1:0",
		workerAddrFile(controlDir),
		bindings,
		func(_ context.Context, _ []string) error { return nil },
		func(_ context.Context) (any, error) { return store.reload(bindings.list()) },
	)
	if err != nil {
		t.Fatalf("startWorkerMgmtServer: %v", err)
	}
	defer func() { _ = server.Close(context.Background()) }()

	setupWorkerRepoConfigAtPath(t, repoBPath, "owner/b", `echo repo-b-reloaded`, "")

	client, err := workerMgmtClientFromControlDir(controlDir)
	if err != nil {
		t.Fatalf("workerMgmtClientFromControlDir: %v", err)
	}
	summary, err := client.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(summary.Repos) != 2 {
		t.Fatalf("summary repos = %d, want 2", len(summary.Repos))
	}
	cfgB, ok := store.get("owner/b")
	if !ok || cfgB.Agents["dev-agent"].Command != "echo repo-b-reloaded" {
		t.Fatalf("repo B config after management reload = %+v, want reloaded command", cfgB)
	}
}
