package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SPAHandler returns an http.Handler that serves the embedded SPA bundle.
//
// Behaviour:
//   - Requests whose path resolves to a real file inside dist/ (e.g.
//     /assets/index-*.js, /favicon.ico, /index.html) are served directly
//     with sensible Content-Type headers.
//   - Any other GET/HEAD request falls back to dist/index.html, so client-
//     side routes deep-linked by the browser hydrate the SPA.
//   - Non-GET/HEAD requests get 405 — the SPA endpoint is read-only.
//
// Auth is intentionally NOT enforced here: callers wrap the handler with
// their existing auth middleware (e.g. api.WrapAuth).
func SPAHandler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// fs.Sub only fails when the embedded path is wrong, which would be
		// a build-time bug rather than a runtime condition.
		panic("webui: embed dist subtree missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return &spaHandler{root: sub, files: fileServer}
}

type spaHandler struct {
	root  fs.FS
	files http.Handler
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	if clean == "/" {
		h.serveIndex(w, r)
		return
	}

	rel := strings.TrimPrefix(clean, "/")
	if rel == "" || strings.Contains(rel, "..") {
		h.serveIndex(w, r)
		return
	}

	if exists, _ := fileExists(h.root, rel); exists {
		h.files.ServeHTTP(w, r)
		return
	}
	h.serveIndex(w, r)
}

func (h *spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(h.root, "index.html")
	if err != nil {
		http.Error(w, "webui not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(data)
}

// fileExists reports whether rel points at a regular file inside fsys.
// Directories don't count — they should fall back to index.html so the
// SPA owns the route rather than the file server's auto-index.
func fileExists(fsys fs.FS, rel string) (bool, error) {
	info, err := fs.Stat(fsys, rel)
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}
