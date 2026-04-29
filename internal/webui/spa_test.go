package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSPAHandler_ServesAssets verifies that real files inside dist/ are
// streamed back with the right body and an asset-friendly Content-Type so
// the browser executes JavaScript bundles.
func TestSPAHandler_ServesAssets(t *testing.T) {
	t.Parallel()
	asset := pickAssetFromEmbed(t)
	srv := httptest.NewServer(SPAHandler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + asset.urlPath)
	if err != nil {
		t.Fatalf("GET %s: %v", asset.urlPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, asset.expectContentType) {
		t.Fatalf("Content-Type = %q, want it to contain %q", ct, asset.expectContentType)
	}
}

// TestSPAHandler_FallbackServesIndex verifies that any unmatched path falls
// back to dist/index.html so client-side routes hydrate the SPA shell.
func TestSPAHandler_FallbackServesIndex(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(SPAHandler())
	t.Cleanup(srv.Close)

	for _, path := range []string{"/", "/some/random/path", "/dashboard", "/sessions/abc-123"} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
				t.Fatalf("Content-Type = %q, want text/html", ct)
			}
		})
	}
}

// TestSPAHandler_DoesNotInterceptAPIRoutes verifies that registering the SPA
// at "/" alongside an API subtree leaves the API path untouched. This is the
// invariant that lets `/api/v1/status` keep its original handler when the
// coordinator mux is wired up.
func TestSPAHandler_DoesNotInterceptAPIRoutes(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle("/", SPAHandler())

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json — SPA handler intercepted the API route", ct)
	}
}

// TestSPAHandler_RejectsNonReadMethods documents the read-only contract:
// POST to a SPA path must not silently fall back to index.html.
func TestSPAHandler_RejectsNonReadMethods(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(SPAHandler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

type spaAsset struct {
	urlPath           string
	expectContentType string
}

// pickAssetFromEmbed walks the embedded dist subtree and picks the first file
// that is not index.html. This keeps the test independent of whether the
// placeholder bundle (just index.html) or a real `make web` bundle (index +
// assets/*.js) is checked in.
func pickAssetFromEmbed(t *testing.T) spaAsset {
	t.Helper()
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	var found spaAsset
	walkErr := fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if path == "index.html" || path == ".keep" {
			return nil
		}
		found = spaAsset{
			urlPath:           "/" + path,
			expectContentType: contentTypeFor(path),
		}
		return fs.SkipAll
	})
	if walkErr != nil {
		t.Fatalf("walk dist: %v", walkErr)
	}
	if found.urlPath == "" {
		// No real asset present in the placeholder bundle. Treat index.html
		// itself as the asset under test — it is still a real file inside
		// dist/ and exercises the "path matched a real file" branch.
		found = spaAsset{urlPath: "/index.html", expectContentType: "text/html"}
	}
	return found
}

func contentTypeFor(path string) string {
	switch {
	case strings.HasSuffix(path, ".js"):
		return "javascript"
	case strings.HasSuffix(path, ".css"):
		return "text/css"
	case strings.HasSuffix(path, ".html"):
		return "text/html"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg"
	default:
		return ""
	}
}
