package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/metrics"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type contextKey string

const workerAuthContextKey contextKey = "worker-auth"

// ServerOptions controls coordinator HTTP behavior relevant to auth.
type ServerOptions struct {
	LoopbackOnly bool
}

// Server exposes the coordinator HTTP API.
type Server struct {
	store *store.Store
	opts  ServerOptions
	mux   *http.ServeMux
}

// NewServer builds the coordinator HTTP API handler.
func NewServer(st *store.Store, opts ServerOptions) *Server {
	s := &Server{
		store: st,
		opts:  opts,
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.handleHealth)
	metrics.NewHandler(s.store).Register(s.mux)
	s.mux.Handle("/api/v1/tasks/poll", s.requireWorkerAuth(http.HandlerFunc(s.handleTaskPoll)))
	s.mux.Handle("/api/v1/tasks/", s.requireWorkerAuth(http.HandlerFunc(s.handleTaskMutation)))
}

func (s *Server) requireWorkerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.LoopbackOnly {
			next.ServeHTTP(w, r)
			return
		}

		token, ok := extractBearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}
		worker, err := s.store.AuthenticateWorkerToken(token)
		if err != nil {
			if errors.Is(err, store.ErrInvalidWorkerToken) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "token lookup failed"})
			return
		}
		ctx := context.WithValue(r.Context(), workerAuthContextKey, worker)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"loopback_only": s.opts.LoopbackOnly,
	})
}

func (s *Server) handleTaskPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	authWorker, _ := workerFromContext(r.Context())
	workerID := strings.TrimSpace(r.URL.Query().Get("worker_id"))
	if authWorker != nil {
		if workerID == "" {
			workerID = authWorker.WorkerID
		}
		if workerID != authWorker.WorkerID {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "worker_id does not match token"})
			return
		}
	}
	if workerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}

	if timeoutRaw := strings.TrimSpace(r.URL.Query().Get("timeout")); timeoutRaw != "" {
		if _, err := time.ParseDuration(timeoutRaw); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid timeout"})
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTaskMutation(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
	taskID, action, ok := strings.Cut(path, "/")
	if !ok || strings.TrimSpace(taskID) == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown task endpoint"})
		return
	}

	switch action {
	case "heartbeat":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "result":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown task endpoint"})
	}
}

func workerFromContext(ctx context.Context) (*store.WorkerAuthRecord, bool) {
	worker, ok := ctx.Value(workerAuthContextKey).(*store.WorkerAuthRecord)
	return worker, ok
}

func extractBearerToken(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if token == "" {
		return "", false
	}
	return token, true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
