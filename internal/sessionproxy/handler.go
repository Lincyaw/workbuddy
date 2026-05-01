package sessionproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LocalHandler is the existing in-process audit handler the coordinator
// already wires up — used as a fall-back when Resolve says the request
// is local (loopback / pre-bundle data on the coordinator host).
type LocalHandler interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}

// Handler is the http.Handler that owns /api/v1/sessions and
// /api/v1/sessions/{id}[/events|/stream] on the coordinator.
type Handler struct {
	resolver *Resolver
	local    LocalHandler
	// authToken is the shared bearer presented to worker audit listeners.
	authToken string
	client    *http.Client
	// listClient is used for fan-out listing calls; a separate http.Client
	// lets us tune its timeout independently from the streaming client.
	listClient *http.Client

	// listing cache (5s TTL).
	cacheMu  sync.Mutex
	listings map[string]listingCacheEntry
	cacheTTL time.Duration
	now      func() time.Time

	// Per-worker fan-out timeout. Slow / offline workers don't block
	// the response — we collect their errors as a sidecar.
	perWorkerTimeout time.Duration
	overallTimeout   time.Duration

	// dispatchHook lets tests count how many times a request actually
	// reached a worker (used to assert cache behaviour). Phase 3 may
	// remove this; it has no production caller.
	dispatchHook func(workerID string)
}

// HandlerConfig bundles handler dependencies.
type HandlerConfig struct {
	Resolver  *Resolver
	Local     LocalHandler
	AuthToken string
	// HTTPClient is used for single-session proxy + stream pass-through.
	// Pass nil for a sane default (http.DefaultTransport, no overall
	// timeout — SSE streams must not be cut off mid-flight). Tests inject.
	HTTPClient *http.Client
	// ListClient is used for /api/v1/sessions fan-out. Pass nil for a
	// default with a per-call timeout. Tests inject.
	ListClient *http.Client
	// CacheTTL controls the in-memory listing cache lifetime. Zero
	// disables caching. Default 5s.
	CacheTTL time.Duration
	// PerWorkerTimeout caps each fan-out call. Default 3s.
	PerWorkerTimeout time.Duration
	// OverallTimeout caps the whole fan-out call. Default 6s.
	OverallTimeout time.Duration
	// Now is the clock; tests inject.
	Now func() time.Time
}

// NewHandler constructs the proxy handler.
func NewHandler(cfg HandlerConfig) *Handler {
	h := &Handler{
		resolver:         cfg.Resolver,
		local:            cfg.Local,
		authToken:        strings.TrimSpace(cfg.AuthToken),
		client:           cfg.HTTPClient,
		listClient:       cfg.ListClient,
		listings:         map[string]listingCacheEntry{},
		cacheTTL:         cfg.CacheTTL,
		perWorkerTimeout: cfg.PerWorkerTimeout,
		overallTimeout:   cfg.OverallTimeout,
		now:              cfg.Now,
	}
	if h.client == nil {
		// No overall timeout: SSE streams must outlive the default
		// 30-second http.Client default in Go's net/http examples (which
		// we're not using anyway, but spell it out for the next reader).
		h.client = &http.Client{}
	}
	if h.listClient == nil {
		h.listClient = &http.Client{Timeout: 5 * time.Second}
	}
	if h.cacheTTL <= 0 {
		h.cacheTTL = 5 * time.Second
	}
	if h.perWorkerTimeout <= 0 {
		h.perWorkerTimeout = 3 * time.Second
	}
	if h.overallTimeout <= 0 {
		h.overallTimeout = 6 * time.Second
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h
}

// Register mounts the proxy on the given mux. It claims:
//
//	/api/v1/sessions          — fan-out listing
//	/api/v1/sessions/         — single-session detail / events / stream
//
// The caller is responsible for any auth wrapping (the coordinator's
// existing api.WrapAuth covers both routes).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/sessions", h.handleList)
	mux.HandleFunc("/api/v1/sessions/", h.handleDetail)
}

// ServeHTTP dispatches to handleList or handleDetail based on the path.
// Lets the handler be wrapped by composite muxes / api.WrapAuth without
// needing two registration calls.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/sessions":
		h.handleList(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/sessions/"):
		h.handleDetail(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleDetail proxies /api/v1/sessions/{id}[/events|/stream] to the
// owning worker, falling back to the local handler when Resolve says
// the data is on this host.
func (h *Handler) handleDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
	sessionID, suffix, _ := strings.Cut(rest, "/")
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		writeJSONError(w, http.StatusNotFound, "session not found", "")
		return
	}

	res, err := h.resolver.Resolve(sessionID)
	switch {
	case errors.Is(err, ErrSessionNotRouted):
		// No row at all. Try the local handler — pre-bundle disk data
		// might still be visible there. If local can't find it either,
		// it returns 404. Phase 3 hook: when the legacy disk-only path
		// is removed, this branch becomes a clean 404 here.
		h.serveLocal(w, r)
		return
	case errors.Is(err, ErrAuditURLMissing):
		writeJSONError(w, http.StatusServiceUnavailable, "worker has no audit_url", workerIDFromError(err))
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "session lookup failed", "")
		return
	}
	if res.Local {
		h.serveLocal(w, r)
		return
	}

	// Build the upstream URL: <audit_url>/api/v1/sessions/{id}[suffix]?<query>
	target := res.AuditURL + "/api/v1/sessions/" + sessionID
	if suffix != "" {
		target += "/" + suffix
	}
	if raw := r.URL.RawQuery; raw != "" {
		target += "?" + raw
	}

	if h.dispatchHook != nil {
		h.dispatchHook(res.WorkerID)
	}

	switch suffix {
	case "stream":
		h.proxyStream(w, r, res.WorkerID, target)
	default:
		h.proxyJSON(w, r, res.WorkerID, target)
	}
}

func (h *Handler) serveLocal(w http.ResponseWriter, r *http.Request) {
	if h.local == nil {
		writeJSONError(w, http.StatusNotFound, "session not found", "")
		return
	}
	h.local.ServeHTTP(w, r)
}

// proxyJSON forwards a non-streaming GET to the worker, copying the
// response body and status. Worker-side response headers are NOT copied
// verbatim — only Content-Type is forwarded so the SPA's existing JSON
// envelope stays clean. Cookies / arbitrary headers from the worker
// have no business reaching the browser through the coordinator.
func (h *Handler) proxyJSON(w http.ResponseWriter, r *http.Request, workerID, target string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "build upstream request failed", workerID)
		return
	}
	if h.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.authToken)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "worker_offline", workerID)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Surface worker_id to the browser for debugging; the SPA can ignore.
	w.Header().Set("X-Workbuddy-Origin-Worker", workerID)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// proxyStream pumps SSE bytes from the worker to the client, flushing
// every chunk so frame boundaries (event: / id: / data:) are preserved.
// Browser disconnects propagate via r.Context() cancellation, which
// cancels the upstream request.
func (h *Handler) proxyStream(w http.ResponseWriter, r *http.Request, workerID, target string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "stream not supported", workerID)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		streamError(w, flusher, "build upstream request failed", workerID)
		return
	}
	if h.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.authToken)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := h.client.Do(req)
	if err != nil {
		streamError(w, flusher, "worker_offline", workerID)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		streamError(w, flusher, "upstream status "+strconv.Itoa(resp.StatusCode), workerID)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Workbuddy-Origin-Worker", workerID)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Pump bytes through with explicit per-chunk flushes. We avoid
	// io.Copy because it batches and a 4 KiB read could merge two SSE
	// frames into one Write — fine for the wire but bad for the
	// "flush per event" guarantee operators rely on for live tailing.
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			return
		}
		if r.Context().Err() != nil {
			return
		}
	}
}

func streamError(w http.ResponseWriter, flusher http.Flusher, reason, workerID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	payload := struct {
		DegradedReason string `json:"degraded_reason"`
		WorkerID       string `json:"worker_id,omitempty"`
	}{DegradedReason: reason, WorkerID: workerID}
	body, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", body)
	flusher.Flush()
}

// writeJSONError emits the {"degraded_reason": ..., "worker_id": ...}
// envelope used by both the single-session and fan-out paths. status
// drives the HTTP status code; degradedReason is the client-facing
// machine label (the SPA renders this directly).
func writeJSONError(w http.ResponseWriter, status int, degradedReason, workerID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"degraded_reason": degradedReason}
	if workerID != "" {
		body["worker_id"] = workerID
	}
	_ = json.NewEncoder(w).Encode(body)
}

// workerIDFromError extracts a worker ID embedded in a wrapped sentinel
// error string of the form "worker <id>: ...". Best-effort: returns
// empty when not present.
func workerIDFromError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	const prefix = "worker "
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return ""
	}
	rest := msg[idx+len(prefix):]
	end := strings.Index(rest, ":")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// listingCacheEntry stores the merged response body for a (filters)
// key, keyed off the request's normalised query.
type listingCacheEntry struct {
	body          []byte
	expiresAt     time.Time
	workerOffline []string
}

// handleList fans the request out across all workers eligible for the
// repo filter, merges the rows, sorts by created_at desc, paginates,
// and serves the result. Slow/offline workers are surfaced via a
// sidecar `worker_offline: ["<id>", ...]` field rather than failing the
// whole call.
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	q := r.URL.Query()

	merged, offline, fromCache, err := h.fanOutListing(r.Context(), q)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "candidate workers lookup failed", "")
		return
	}
	if fromCache {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Workbuddy-Cache", "hit")
		bareBody, mErr := json.Marshal(merged)
		if mErr != nil {
			writeJSONError(w, http.StatusInternalServerError, "encode response failed", "")
			return
		}
		if len(offline) > 0 {
			w.Header().Set("X-Workbuddy-Worker-Offline", strings.Join(offline, ","))
		}
		_, _ = w.Write(bareBody)
		return
	}

	bareBody, err := json.Marshal(merged)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "encode response failed", "")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if len(offline) > 0 {
		w.Header().Set("X-Workbuddy-Worker-Offline", strings.Join(offline, ","))
	}
	// Backwards-compat with the SPA: the existing reader expects a bare
	// JSON array, not an envelope. We serve the array directly and fold
	// worker_offline into a custom response header. The frontend reads
	// the header in fetchSessions to render an "N workers offline" banner.
	_, _ = w.Write(bareBody)
}

// fanOutListing performs the cross-worker session fan-out used by
// /api/v1/sessions and by coordinator-internal callers (issue-detail).
// Returns the merged rows, the list of offline worker IDs, a flag
// indicating the result was served from cache (caller may want to mark
// the response with X-Workbuddy-Cache), and any setup error.
//
// On a partial response (some workers offline), the result is NOT
// cached — the next call should retry the offline ones in case they
// recovered. On a fully successful response, the merged rows are cached
// for cacheTTL.
func (h *Handler) fanOutListing(parent context.Context, q map[string][]string) (rows []map[string]any, offline []string, fromCache bool, err error) {
	repo := firstQueryValue(q, "repo")
	agent := firstQueryValue(q, "agent")
	issue := firstQueryValue(q, "issue")
	limitRaw := firstQueryValue(q, "limit")
	offsetRaw := firstQueryValue(q, "offset")

	cacheKey := fanoutCacheKey(repo, agent, issue, limitRaw, offsetRaw)
	if entry, ok := h.cacheGet(cacheKey); ok {
		var cached []map[string]any
		if jErr := json.Unmarshal(entry.body, &cached); jErr == nil {
			return cached, entry.workerOffline, true, nil
		}
	}

	workers, lookupErr := h.resolver.CandidateWorkers(repo)
	if lookupErr != nil {
		return nil, nil, false, lookupErr
	}

	type workerResult struct {
		workerID string
		rows     []map[string]any
		err      error
	}

	ctx, cancel := context.WithTimeout(parent, h.overallTimeout)
	defer cancel()

	results := make(chan workerResult, len(workers))
	var wg sync.WaitGroup
	for _, worker := range workers {
		auditURL := strings.TrimRight(strings.TrimSpace(worker.AuditURL), "/")
		if auditURL == "" {
			// Worker registered but did not advertise an audit_url.
			// Treat as offline: a Phase 1 worker with --audit-listen
			// disabled has no session API to fan-out to.
			results <- workerResult{workerID: worker.ID, err: ErrAuditURLMissing}
			continue
		}
		wg.Add(1)
		go func(workerID, base string) {
			defer wg.Done()
			rows, ferr := h.fetchWorkerListing(ctx, workerID, base, q)
			if h.dispatchHook != nil {
				h.dispatchHook(workerID)
			}
			results <- workerResult{workerID: workerID, rows: rows, err: ferr}
		}(worker.ID, auditURL)
	}
	go func() { wg.Wait(); close(results) }()

	merged := make([]map[string]any, 0, 32)
	seen := make(map[string]struct{})
	offline = make([]string, 0)
	hadAnyError := false
	for res := range results {
		if res.err != nil {
			hadAnyError = true
			offline = append(offline, res.workerID)
			continue
		}
		for _, row := range res.rows {
			id, _ := row["session_id"].(string)
			if id == "" {
				merged = append(merged, row)
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			merged = append(merged, row)
		}
	}

	sort.SliceStable(merged, func(i, j int) bool {
		return rowCreatedAt(merged[i]).After(rowCreatedAt(merged[j]))
	})

	// Apply limit/offset on the merged result. Each worker already had
	// limit/offset pushed down so we have at most N*<limit> rows here.
	limit := parseIntOr(limitRaw, 0)
	offset := parseIntOr(offsetRaw, 0)
	if offset > 0 {
		if offset >= len(merged) {
			merged = merged[:0]
		} else {
			merged = merged[offset:]
		}
	}
	if limit > 0 && limit < len(merged) {
		merged = merged[:limit]
	}

	if len(offline) > 0 {
		sort.Strings(offline)
	}

	if !hadAnyError {
		bareBody, mErr := json.Marshal(merged)
		if mErr == nil {
			h.cacheSet(cacheKey, listingCacheEntry{
				body:          bareBody,
				expiresAt:     h.now().Add(h.cacheTTL),
				workerOffline: offline,
			})
		}
	}

	return merged, offline, false, nil
}

// ListSessionsForRepoIssue is a programmatic entry point onto the same
// fan-out used by /api/v1/sessions. Used by coordinator-internal callers
// (the issue-detail endpoint) that need the merged rows directly without
// going through the HTTP wrapper. Errors during candidate lookup are
// returned; per-worker errors are folded into the offline list, same as
// the HTTP path.
//
// Phase 3 (REQ-122): added so the coordinator's issue-detail handler
// can stop reading from the now-dropped local sessions table.
func (h *Handler) ListSessionsForRepoIssue(ctx context.Context, repo string, issueNum int) ([]map[string]any, []string, error) {
	q := map[string][]string{"repo": {repo}}
	if issueNum > 0 {
		q["issue"] = []string{strconv.Itoa(issueNum)}
	}
	rows, offline, _, err := h.fanOutListing(ctx, q)
	return rows, offline, err
}

// firstQueryValue returns q[key][0] or "" — pulls the first non-empty
// value out of a parsed query map.
func firstQueryValue(q map[string][]string, key string) string {
	if vs, ok := q[key]; ok && len(vs) > 0 {
		return strings.TrimSpace(vs[0])
	}
	return ""
}

// fetchWorkerListing pushes the same filters down to one worker.
func (h *Handler) fetchWorkerListing(ctx context.Context, workerID, auditURL string, query map[string][]string) ([]map[string]any, error) {
	upstream := auditURL + "/api/v1/sessions"
	values := make([]string, 0, 8)
	for _, key := range []string{"repo", "agent", "issue", "limit", "offset"} {
		if vs, ok := query[key]; ok && len(vs) > 0 && vs[0] != "" {
			values = append(values, key+"="+vs[0])
		}
	}
	if len(values) > 0 {
		upstream += "?" + strings.Join(values, "&")
	}

	cctx, cancel := context.WithTimeout(ctx, h.perWorkerTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, upstream, nil)
	if err != nil {
		return nil, fmt.Errorf("worker %s: build request: %w", workerID, err)
	}
	if h.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.authToken)
	}
	resp, err := h.listClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("worker %s: %w", workerID, ErrWorkerOffline)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker %s: status %d: %w", workerID, resp.StatusCode, ErrWorkerOffline)
	}
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("worker %s: decode body: %w", workerID, err)
	}
	return rows, nil
}

func (h *Handler) cacheGet(key string) (listingCacheEntry, bool) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	entry, ok := h.listings[key]
	if !ok {
		return listingCacheEntry{}, false
	}
	if h.now().After(entry.expiresAt) {
		delete(h.listings, key)
		return listingCacheEntry{}, false
	}
	return entry, true
}

func (h *Handler) cacheSet(key string, entry listingCacheEntry) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	h.listings[key] = entry
}

// InvalidateCache drops the in-memory listing cache. The coordinator
// can call this from the worker-registered eventlog hook for tighter
// freshness; without it the 5s TTL covers everyday churn.
//
// Phase 3: wire to eventlog "worker_registered" so a fresh worker is
// visible immediately, and drop this exported method if unused.
func (h *Handler) InvalidateCache() {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	h.listings = map[string]listingCacheEntry{}
}

func fanoutCacheKey(parts ...string) string {
	return strings.Join(parts, "|")
}

func parseIntOr(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return fallback
	}
	return v
}

// rowCreatedAt returns the timestamp from a session-list row, or zero
// if absent. Workers serialise time.Time as RFC3339, which JSON decodes
// as a string into a map[string]any.
func rowCreatedAt(row map[string]any) time.Time {
	if row == nil {
		return time.Time{}
	}
	raw, ok := row["created_at"].(string)
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}
