package labelwriter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// fakeRegistration returns a registrationLookup backed by an in-memory map.
func fakeRegistration(configByRepo map[string]string) registrationLookup {
	return func(repo string) (*store.RepoRegistrationRecord, error) {
		cfg, ok := configByRepo[repo]
		if !ok {
			return nil, nil
		}
		return &store.RepoRegistrationRecord{Repo: repo, ConfigJSON: cfg}, nil
	}
}

type capturedExec struct {
	name string
	args []string
}

func newWriterWithCapture() (*Writer, *capturedExec) {
	cap := &capturedExec{}
	w := &Writer{
		lookup:   func(string) (*store.RepoRegistrationRecord, error) { return nil, nil },
		lookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			cap.name = name
			cap.args = args
			return []byte("ok"), nil
		},
	}
	return w, cap
}

func TestApplyNextLabel_EmptyLabelIsNoop(t *testing.T) {
	w, cap := newWriterWithCapture()
	if err := w.ApplyNextLabel(context.Background(), "org/repo", 7, ""); err != nil {
		t.Fatalf("empty label should be no-op, got err: %v", err)
	}
	if cap.name != "" {
		t.Fatalf("empty label invoked exec: name=%q args=%v", cap.name, cap.args)
	}
}

func TestApplyNextLabel_GitHubGHArgs(t *testing.T) {
	w, cap := newWriterWithCapture()
	// No registration -> defaults to GitHub host_kind.
	if err := w.ApplyNextLabel(context.Background(), "org/repo", 42, "status:in-progress"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cap.name != "/usr/bin/gh" {
		t.Fatalf("expected gh binary, got %q", cap.name)
	}
	want := []string{"issue", "edit", "42", "--repo", "org/repo", "--add-label", "status:in-progress"}
	if !equalStrings(cap.args, want) {
		t.Fatalf("gh args mismatch:\n got %v\nwant %v", cap.args, want)
	}
}

func TestApplyNextLabel_HostKindGitHubExplicit(t *testing.T) {
	w, cap := newWriterWithCapture()
	w.lookup = fakeRegistration(map[string]string{
		"org/repo": `{"host_kind":"github"}`,
	})
	if err := w.ApplyNextLabel(context.Background(), "org/repo", 9, "status:done"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cap.args[len(cap.args)-1] != "status:done" {
		t.Fatalf("expected label as last arg, got %v", cap.args)
	}
}

func TestApplyNextLabel_GHMissingOnPath(t *testing.T) {
	w := &Writer{
		lookup:   func(string) (*store.RepoRegistrationRecord, error) { return nil, nil },
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
		run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("run should not be called when lookPath fails")
			return nil, nil
		},
	}
	err := w.ApplyNextLabel(context.Background(), "org/repo", 1, "x")
	if err == nil || !strings.Contains(err.Error(), "gh CLI not found") {
		t.Fatalf("expected gh-not-found error, got %v", err)
	}
}

func TestApplyNextLabel_GHRunError(t *testing.T) {
	w, _ := newWriterWithCapture()
	w.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("bad credentials"), errors.New("exit 1")
	}
	err := w.ApplyNextLabel(context.Background(), "org/repo", 3, "x")
	if err == nil || !strings.Contains(err.Error(), "bad credentials") {
		t.Fatalf("expected wrapped output in error, got %v", err)
	}
}

func TestApplyNextLabel_LookupError(t *testing.T) {
	boom := errors.New("db down")
	w, _ := newWriterWithCapture()
	w.lookup = func(string) (*store.RepoRegistrationRecord, error) { return nil, boom }
	err := w.ApplyNextLabel(context.Background(), "org/repo", 4, "x")
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("expected wrapped lookup error, got %v", err)
	}
}

func TestApplyNextLabel_UnsupportedHostKind(t *testing.T) {
	w, _ := newWriterWithCapture()
	w.lookup = fakeRegistration(map[string]string{
		"org/repo": `{"host_kind":"bitbucket"}`,
	})
	err := w.ApplyNextLabel(context.Background(), "org/repo", 5, "x")
	if err == nil || !strings.Contains(err.Error(), "unsupported host_kind") {
		t.Fatalf("expected unsupported host_kind error, got %v", err)
	}
}

func TestApplyNextLabel_MalformedConfigDefaultsToGitHub(t *testing.T) {
	w, cap := newWriterWithCapture()
	w.lookup = fakeRegistration(map[string]string{
		"org/repo": `{not json`,
	})
	if err := w.ApplyNextLabel(context.Background(), "org/repo", 6, "y"); err != nil {
		t.Fatalf("malformed config should fall back to gh, got err %v", err)
	}
	if cap.name == "" {
		t.Fatalf("expected gh to be invoked")
	}
}

// --- Gitea path ---

type fakeHTTP struct {
	req      *http.Request
	body     []byte
	respCode int
	respBody string
	err      error
}

func (f *fakeHTTP) Do(req *http.Request) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.req = req
	if req.Body != nil {
		f.body, _ = io.ReadAll(req.Body)
	}
	code := f.respCode
	if code == 0 {
		code = http.StatusOK
	}
	body := f.respBody
	if body == "" {
		body = "[]"
	}
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func TestApplyNextLabel_GiteaHappyPath(t *testing.T) {
	hc := &fakeHTTP{}
	w := &Writer{
		lookup: fakeRegistration(map[string]string{
			"org/repo": `{"host_kind":"gitea","gitea_base_url":"https://gitea.example.com/"}`,
		}),
		http:       hc,
		giteaToken: "secret-token",
	}
	if err := w.ApplyNextLabel(context.Background(), "org/repo", 11, "status:in-progress"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if hc.req == nil {
		t.Fatal("expected HTTP request to be issued")
	}
	if got, want := hc.req.URL.String(), "https://gitea.example.com/api/v1/repos/org/repo/issues/11/labels"; got != want {
		t.Fatalf("url mismatch:\n got %s\nwant %s", got, want)
	}
	if got := hc.req.Header.Get("Authorization"); got != "token secret-token" {
		t.Fatalf("auth header mismatch: %q", got)
	}
	if got := hc.req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type mismatch: %q", got)
	}
	if !strings.Contains(string(hc.body), `"status:in-progress"`) {
		t.Fatalf("body missing label: %s", hc.body)
	}
}

func TestApplyNextLabel_GiteaMissingBaseURL(t *testing.T) {
	w := &Writer{
		lookup: fakeRegistration(map[string]string{
			"org/repo": `{"host_kind":"gitea"}`,
		}),
		http:       &fakeHTTP{},
		giteaToken: "tok",
	}
	err := w.ApplyNextLabel(context.Background(), "org/repo", 1, "x")
	if err == nil || !strings.Contains(err.Error(), "gitea_base_url missing") {
		t.Fatalf("expected gitea_base_url error, got %v", err)
	}
}

func TestApplyNextLabel_GiteaMissingToken(t *testing.T) {
	t.Setenv("GITEA_TOKEN", "")
	w := &Writer{
		lookup: fakeRegistration(map[string]string{
			"org/repo": `{"host_kind":"gitea","gitea_base_url":"https://g.example/"}`,
		}),
		http: &fakeHTTP{},
	}
	err := w.ApplyNextLabel(context.Background(), "org/repo", 1, "x")
	if err == nil || !strings.Contains(err.Error(), "GITEA_TOKEN not set") {
		t.Fatalf("expected token-missing error, got %v", err)
	}
}

func TestApplyNextLabel_GiteaErrorStatus(t *testing.T) {
	hc := &fakeHTTP{respCode: 422, respBody: `{"message":"bad label"}`}
	w := &Writer{
		lookup: fakeRegistration(map[string]string{
			"org/repo": `{"host_kind":"gitea","gitea_base_url":"https://g.example"}`,
		}),
		http:       hc,
		giteaToken: "tok",
	}
	err := w.ApplyNextLabel(context.Background(), "org/repo", 1, "x")
	if err == nil || !strings.Contains(err.Error(), "status 422") {
		t.Fatalf("expected status 422 error, got %v", err)
	}
}

func equalStrings(a, b []string) bool {
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
