// Package webui provides HTTP handlers for the session viewer web UI.
package webui

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// Handler serves the session viewer web UI.
type Handler struct {
	store     *store.Store
	listTmpl  *template.Template
	detailTmpl *template.Template
	notFoundTmpl *template.Template
}

// NewHandler creates a Handler backed by the given store.
func NewHandler(st *store.Store) *Handler {
	funcMap := template.FuncMap{
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "..."
		},
	}

	h := &Handler{store: st}
	h.listTmpl = template.Must(template.New("list").Funcs(funcMap).Parse(listHTML))
	h.detailTmpl = template.Must(template.New("detail").Funcs(funcMap).Parse(detailHTML))
	h.notFoundTmpl = template.Must(template.New("notfound").Parse(notFoundHTML))
	return h
}

// Register adds the session viewer routes to the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/sessions", h.handleList)
	mux.HandleFunc("/sessions/", h.handleDetail)
}

// handleList renders the session list page with optional filtering.
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.SessionFilter{
		Repo:      q.Get("repo"),
		AgentName: q.Get("agent"),
	}
	if issueStr := q.Get("issue"); issueStr != "" {
		if n, err := strconv.Atoi(issueStr); err == nil {
			filter.IssueNum = n
		}
	}

	sessions, err := h.store.ListAgentSessions(filter)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	data := listData{
		Sessions: sessions,
		Filter:   filter,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.listTmpl.Execute(w, data); err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
	}
}

// handleDetail renders a single session detail page.
func (h *Handler) handleDetail(w http.ResponseWriter, r *http.Request) {
	// Extract session ID from path: /sessions/{sessionID}
	sessionID := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if sessionID == "" {
		http.Redirect(w, r, "/sessions", http.StatusFound)
		return
	}

	sess, err := h.store.GetAgentSession(sessionID)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if sess == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_ = h.notFoundTmpl.Execute(w, sessionID)
		return
	}

	// Look up the task record for extra metadata (status, duration).
	var taskStatus string
	if sess.TaskID != "" {
		tasks, err := h.store.QueryTasks("")
		if err == nil {
			for _, t := range tasks {
				if t.ID == sess.TaskID {
					taskStatus = t.Status
					break
				}
			}
		}
	}

	data := detailData{
		Session:    *sess,
		TaskStatus: taskStatus,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.detailTmpl.Execute(w, data); err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
	}
}

// listData is the template data for the session list page.
type listData struct {
	Sessions []store.AgentSession
	Filter   store.SessionFilter
}

// detailData is the template data for the session detail page.
type detailData struct {
	Session    store.AgentSession
	TaskStatus string
}

// SessionURL returns the URL path for a session detail page.
func SessionURL(sessionID string) string {
	return fmt.Sprintf("/sessions/%s", sessionID)
}
