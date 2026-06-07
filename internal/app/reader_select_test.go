package app

import (
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/giteareader"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// stubGHReader is a no-op poller.GHReader used as the PollerManager's default
// (GitHub) reader so we can assert identity-based selection.
type stubGHReader struct{}

func (stubGHReader) ListIssues(string) ([]poller.Issue, error) { return nil, nil }
func (stubGHReader) ListPRs(string) ([]poller.PR, error)       { return nil, nil }
func (stubGHReader) CheckRepoAccess(string) error              { return nil }
func (stubGHReader) ReadIssue(string, int) (poller.IssueDetails, error) {
	return poller.IssueDetails{}, nil
}

func newSelectPM() (*PollerManager, *stubGHReader) {
	gh := &stubGHReader{}
	return &PollerManager{ghReader: gh}, gh
}

func TestReaderForRepo_DefaultsToGHReader(t *testing.T) {
	pm, gh := newSelectPM()

	cases := []store.RepoRegistrationRecord{
		{Repo: "org/repo"}, // no config -> default
		{Repo: "org/repo", ConfigJSON: `{"host_kind":"github"}`},  // explicit github
		{Repo: "org/repo", ConfigJSON: `{not json`},               // malformed -> default
		{Repo: "org/repo", ConfigJSON: `{"host_kind":"unknown"}`}, // unsupported -> default
	}
	for _, rec := range cases {
		reader, err := pm.readerForRepo(rec)
		if err != nil {
			t.Fatalf("config %q: unexpected err: %v", rec.ConfigJSON, err)
		}
		if reader != poller.GHReader(gh) {
			t.Fatalf("config %q: expected the default gh reader, got %T", rec.ConfigJSON, reader)
		}
	}
}

func TestReaderForRepo_GiteaSelectsGiteaReader(t *testing.T) {
	t.Setenv("GITEA_TOKEN", "secret")
	pm, _ := newSelectPM()

	rec := store.RepoRegistrationRecord{
		Repo:       "org/repo",
		ConfigJSON: `{"host_kind":"gitea","gitea_base_url":"https://gitea.example.com"}`,
	}
	reader, err := pm.readerForRepo(rec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, ok := reader.(*giteareader.Reader); !ok {
		t.Fatalf("expected *giteareader.Reader, got %T", reader)
	}
}

func TestReaderForRepo_GiteaMissingBaseURL(t *testing.T) {
	t.Setenv("GITEA_TOKEN", "secret")
	pm, _ := newSelectPM()

	rec := store.RepoRegistrationRecord{Repo: "org/repo", ConfigJSON: `{"host_kind":"gitea"}`}
	_, err := pm.readerForRepo(rec)
	if err == nil || !strings.Contains(err.Error(), "gitea_base_url is missing") {
		t.Fatalf("expected gitea_base_url error, got %v", err)
	}
}

func TestReaderForRepo_GiteaMissingToken(t *testing.T) {
	t.Setenv("GITEA_TOKEN", "")
	pm, _ := newSelectPM()

	rec := store.RepoRegistrationRecord{
		Repo:       "org/repo",
		ConfigJSON: `{"host_kind":"gitea","gitea_base_url":"https://gitea.example.com"}`,
	}
	_, err := pm.readerForRepo(rec)
	if err == nil || !strings.Contains(err.Error(), "GITEA_TOKEN is not set") {
		t.Fatalf("expected GITEA_TOKEN error, got %v", err)
	}
}
