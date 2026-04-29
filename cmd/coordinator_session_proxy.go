package cmd

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/store"
)

type coordinatorSessionProxy struct {
	store      *store.Store
	sharedAuth string
	client     *http.Client
}

func newCoordinatorSessionProxy(st *store.Store, sharedAuth string) http.Handler {
	return &coordinatorSessionProxy{
		store:      st,
		sharedAuth: strings.TrimSpace(sharedAuth),
		client:     &http.Client{},
	}
}

func (p *coordinatorSessionProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p == nil || p.store == nil {
		http.NotFound(w, r)
		return
	}
	workerID, sessionPath, ok := parseWorkerSessionProxyPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	worker, err := p.store.GetWorker(workerID)
	if err != nil {
		http.Error(w, "failed to query worker", http.StatusInternalServerError)
		return
	}
	if worker == nil || strings.TrimSpace(worker.MgmtBaseURL) == "" {
		http.NotFound(w, r)
		return
	}

	targetURL := strings.TrimRight(worker.MgmtBaseURL, "/") + sessionPath
	if raw := r.URL.RawQuery; raw != "" {
		targetURL += "?" + raw
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "failed to build worker session request", http.StatusBadGateway)
		return
	}
	req.Header = r.Header.Clone()
	if p.sharedAuth != "" {
		req.Header.Set("Authorization", "Bearer "+p.sharedAuth)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		http.Error(w, "worker session viewer unavailable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	rewritePrefix := "/workers/" + url.PathEscape(workerID) + "/sessions"
	for key, values := range resp.Header {
		for _, value := range values {
			if strings.EqualFold(key, "Location") && strings.HasPrefix(value, "/sessions") {
				value = rewriteWorkerSessionHTML(workerID, value)
			}
			w.Header().Add(key, value)
		}
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "failed to read worker session response", http.StatusBadGateway)
			return
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write([]byte(strings.ReplaceAll(string(body), "\"/sessions", "\""+rewritePrefix)))
		return
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func parseWorkerSessionProxyPath(path string) (workerID, sessionPath string, ok bool) {
	if !strings.HasPrefix(path, "/workers/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, "/workers/")
	workerPart, suffix, found := strings.Cut(rest, "/sessions")
	if !found || workerPart == "" {
		return "", "", false
	}
	workerID, err := url.PathUnescape(workerPart)
	if err != nil || strings.TrimSpace(workerID) == "" {
		return "", "", false
	}
	if suffix != "" && !strings.HasPrefix(suffix, "/") {
		return "", "", false
	}
	return workerID, "/sessions" + suffix, true
}

func rewriteWorkerSessionHTML(workerID, value string) string {
	return strings.ReplaceAll(value, "/sessions", "/workers/"+url.PathEscape(workerID)+"/sessions")
}
