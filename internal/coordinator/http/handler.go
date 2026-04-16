package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

const (
	defaultLongPoll = 30 * time.Second
	defaultLease    = 45 * time.Second
	pollInterval    = 250 * time.Millisecond
)

type Handler struct {
	store *store.Store
}

func NewHandler(st *store.Store) *Handler {
	return &Handler{store: st}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/tasks/claim", h.handleClaim)
	mux.HandleFunc("/v1/tasks/", h.handleTaskAction)
}

type claimRequest struct {
	WorkerID       string   `json:"worker_id"`
	Roles          []string `json:"roles"`
	IdempotencyKey string   `json:"idempotency_key"`
	LongPollSecs   int      `json:"long_poll_seconds"`
	LeaseSecs      int      `json:"lease_seconds"`
}

type taskResponse struct {
	ID          string   `json:"id"`
	Repo        string   `json:"repo"`
	IssueNum    int      `json:"issue_num"`
	AgentName   string   `json:"agent_name"`
	Role        string   `json:"role"`
	Runtime     string   `json:"runtime"`
	Workflow    string   `json:"workflow"`
	State       string   `json:"state"`
	Status      string   `json:"status"`
	WorkerID    string   `json:"worker_id,omitempty"`
	ExitCode    int      `json:"exit_code,omitempty"`
	SessionRefs []string `json:"session_refs,omitempty"`
}

type workerRequest struct {
	WorkerID    string   `json:"worker_id"`
	LeaseSecs   int      `json:"lease_seconds"`
	ExitCode    int      `json:"exit_code"`
	SessionRefs []string `json:"session_refs"`
}

func taskToResponse(task *store.TaskRecord) taskResponse {
	resp := taskResponse{
		ID:        task.ID,
		Repo:      task.Repo,
		IssueNum:  task.IssueNum,
		AgentName: task.AgentName,
		Role:      task.Role,
		Runtime:   task.Runtime,
		Workflow:  task.Workflow,
		State:     task.State,
		Status:    task.Status,
		WorkerID:  task.WorkerID,
		ExitCode:  task.ExitCode,
	}
	if task.SessionRefs != "" {
		var refs []string
		if err := json.Unmarshal([]byte(task.SessionRefs), &refs); err == nil {
			resp.SessionRefs = refs
		}
	}
	return resp
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func (h *Handler) handleClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req claimRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.WorkerID) == "" {
		writeError(w, http.StatusBadRequest, "worker_id is required")
		return
	}
	longPoll := time.Duration(req.LongPollSecs) * time.Second
	if longPoll <= 0 {
		longPoll = defaultLongPoll
	}
	lease := time.Duration(req.LeaseSecs) * time.Second
	if lease <= 0 {
		lease = defaultLease
	}
	ctx, cancel := context.WithTimeout(r.Context(), longPoll)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		task, err := h.store.ClaimNextTask(req.WorkerID, req.Roles, req.IdempotencyKey, lease)
		switch {
		case err == nil && task != nil:
			writeJSON(w, http.StatusOK, taskToResponse(task))
			return
		case err == nil:
		case errors.Is(err, store.ErrTaskClaimConflict):
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		select {
		case <-ctx.Done():
			w.WriteHeader(http.StatusNoContent)
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) handleTaskAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	taskID, action, ok := strings.Cut(path, "/")
	if !ok || taskID == "" {
		http.NotFound(w, r)
		return
	}

	var req workerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.WorkerID) == "" {
		writeError(w, http.StatusBadRequest, "worker_id is required")
		return
	}
	lease := time.Duration(req.LeaseSecs) * time.Second
	if lease <= 0 {
		lease = defaultLease
	}

	var err error
	switch action {
	case "ack":
		err = h.store.AckTask(taskID, req.WorkerID, lease)
	case "heartbeat":
		err = h.store.HeartbeatTask(taskID, req.WorkerID, lease)
	case "complete":
		sessionRefs, marshalErr := json.Marshal(req.SessionRefs)
		if marshalErr != nil {
			writeError(w, http.StatusBadRequest, "invalid session_refs")
			return
		}
		err = h.store.CompleteTask(taskID, req.WorkerID, req.ExitCode, string(sessionRefs))
	default:
		http.NotFound(w, r)
		return
	}

	switch {
	case err == nil:
		task, getErr := h.store.GetTask(taskID)
		if getErr != nil {
			writeError(w, http.StatusInternalServerError, getErr.Error())
			return
		}
		if task == nil {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeJSON(w, http.StatusOK, taskToResponse(task))
	case errors.Is(err, store.ErrTaskNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrTaskNotClaimedByWorker), errors.Is(err, store.ErrTaskClaimConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrTaskAlreadyCompleted):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
