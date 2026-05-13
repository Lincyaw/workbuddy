package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitInit creates a bare upstream and a working clone pre-populated with
// one commit so subsequent CommitAndPush calls have a sane starting
// point. Returns (workdir, bareRepoPath).
func gitInit(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "upstream.git")
	work := filepath.Join(root, "work")
	mustRun(t, "", "git", "init", "--bare", bare)
	mustRun(t, "", "git", "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustRun(t, work, "git", "config", "user.name", "init")
	mustRun(t, work, "git", "config", "user.email", "init@example.test")
	mustRun(t, work, "git", "add", "-A")
	mustRun(t, work, "git", "commit", "-m", "init")
	mustRun(t, work, "git", "remote", "add", "origin", bare)
	mustRun(t, work, "git", "push", "origin", "main")
	return work, bare
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, string(out))
	}
}

func TestCommitAndPush_HappyPath(t *testing.T) {
	work, bare := gitInit(t)

	// Drop a fresh file into the worktree — simulates AgentM having
	// applied its artifact.
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("agentm output\n"), 0644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	ctx := context.Background()
	branch := "workbuddy/issue-330"
	err := CommitAndPush(ctx, work, branch, "workbuddy: ship issue #330", DefaultBotAuthor)
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}

	// Verify the branch reached the bare upstream.
	cmd := exec.Command("git", "--git-dir", bare, "branch", "--list", branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch on bare: %v: %s", err, string(out))
	}
	if !strings.Contains(string(out), branch) {
		t.Fatalf("expected branch %q on upstream, got %q", branch, string(out))
	}

	// Verify author identity on the tip commit.
	cmd = exec.Command("git", "--git-dir", bare, "log", branch, "-1", "--format=%an <%ae>")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, string(out))
	}
	want := DefaultBotAuthor.Name + " <" + DefaultBotAuthor.Email + ">"
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("author = %q, want %q", got, want)
	}
}

func TestCommitAndPush_NoChanges(t *testing.T) {
	work, _ := gitInit(t)
	ctx := context.Background()
	err := CommitAndPush(ctx, work, "workbuddy/issue-330", "noop", DefaultBotAuthor)
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("err = %v, want ErrNoChanges", err)
	}
}

func TestCommitAndPush_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		path string
		br   string
		msg  string
		auth Author
	}{
		{"empty path", "", "b", "m", DefaultBotAuthor},
		{"empty branch", "/tmp", "", "m", DefaultBotAuthor},
		{"empty msg", "/tmp", "b", "", DefaultBotAuthor},
		{"empty author name", "/tmp", "b", "m", Author{Email: "x@y"}},
		{"empty author email", "/tmp", "b", "m", Author{Name: "x"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := CommitAndPush(ctx, tc.path, tc.br, tc.msg, tc.auth)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestCommitAndPush_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	err := CommitAndPush(context.Background(), dir, "b", "m", DefaultBotAuthor)
	if err == nil {
		t.Fatal("expected error for non-git dir")
	}
}

// fakeRunner records each invocation and returns scripted results so we
// can assert OpenPR's argv shape without needing a real `gh` install.
type fakeRunner struct {
	calls []fakeCall
	// next is consumed left-to-right; each entry is the (stdout,
	// stderr, err) returned for the matching call.
	next []fakeResult
}

type fakeCall struct {
	dir  string
	name string
	args []string
}

type fakeResult struct {
	stdout string
	stderr string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, dir string, _ []string, name string, args ...string) (string, string, error) {
	f.calls = append(f.calls, fakeCall{dir: dir, name: name, args: append([]string{}, args...)})
	if len(f.next) == 0 {
		return "", "", nil
	}
	res := f.next[0]
	f.next = f.next[1:]
	return res.stdout, res.stderr, res.err
}

func TestOpenPR_HappyPath(t *testing.T) {
	fr := &fakeRunner{next: []fakeResult{
		{stdout: "https://github.com/Lincyaw/workbuddy/pull/999\n"},
	}}
	c := Client{Runner: fr, GHBinary: "gh"}
	url, err := c.OpenPR(context.Background(), "Lincyaw/workbuddy", "workbuddy/issue-330", "title", "body referencing #330")
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if url != "https://github.com/Lincyaw/workbuddy/pull/999" {
		t.Fatalf("url = %q", url)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(fr.calls))
	}
	call := fr.calls[0]
	if call.name != "gh" {
		t.Fatalf("name = %q", call.name)
	}
	want := []string{"pr", "create", "--repo", "Lincyaw/workbuddy", "--head", "workbuddy/issue-330", "--title", "title", "--body", "body referencing #330"}
	if !stringSlicesEqual(call.args, want) {
		t.Fatalf("args = %v\nwant %v", call.args, want)
	}
}

func TestOpenPR_GHFailure(t *testing.T) {
	fr := &fakeRunner{next: []fakeResult{
		{stderr: "GraphQL: not authorized", err: errors.New("exit 1")},
	}}
	c := Client{Runner: fr}
	_, err := c.OpenPR(context.Background(), "Lincyaw/workbuddy", "b", "t", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("error should carry stderr, got %v", err)
	}
}

func TestOpenPR_NoURL(t *testing.T) {
	fr := &fakeRunner{next: []fakeResult{{stdout: "   \n"}}}
	c := Client{Runner: fr}
	_, err := c.OpenPR(context.Background(), "r/r", "b", "t", "")
	if err == nil {
		t.Fatal("expected error when gh returns no URL")
	}
}

func TestOpenPR_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, repo, branch, title string
	}{
		{"empty repo", "", "b", "t"},
		{"empty branch", "r/r", "", "t"},
		{"empty title", "r/r", "b", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := OpenPR(ctx, tc.repo, tc.branch, tc.title, ""); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
