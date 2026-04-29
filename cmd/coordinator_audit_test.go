package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/app"
	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func seedCoordinatorAuditDB(t *testing.T, dbPath string) {
	t.Helper()

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.DB().Exec(
		`INSERT INTO issue_cache (repo, issue_num, labels, body, state) VALUES (?, ?, ?, ?, ?)`,
		"owner/repo-a", 11, `["workbuddy","status:developing"]`, "body", "open",
	); err != nil {
		t.Fatalf("insert issue_cache: %v", err)
	}

	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-11",
		Repo:      "owner/repo-a",
		IssueNum:  11,
		AgentName: "dev-agent",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	eventID, err := st.InsertEvent(store.Event{
		Type:     "dispatch",
		Repo:     "owner/repo-a",
		IssueNum: 11,
		Payload:  `{"task_id":"task-11"}`,
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE events SET ts = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), eventID); err != nil {
		t.Fatalf("update event ts: %v", err)
	}
}

func TestCoordinatorExposesAuditEndpoints(t *testing.T) {
	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	seedCoordinatorAuditDB(t, dbPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: time.Second,
			dbPath:       dbPath,
		}, nil, ctx)
	}()
	waitForHealth(t, port)

	client := &http.Client{Timeout: 5 * time.Second}

	eventsResp := getCoordinator(t, client, fmt.Sprintf("http://localhost:%d/events?repo=owner/repo-a&type=dispatch&since=2000-01-01T00:00:00Z", port), "")
	defer func() { _ = eventsResp.Body.Close() }()
	if eventsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events status = %d", eventsResp.StatusCode)
	}
	var events audit.EventsResponse
	if err := json.NewDecoder(eventsResp.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(events.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(events.Events))
	}
	if got := events.Events[0].Type; got != "dispatch" {
		t.Fatalf("event type = %q, want dispatch", got)
	}

	tasksResp := getCoordinator(t, client, fmt.Sprintf("http://localhost:%d/tasks?repo=owner/repo-a&status=pending", port), "")
	defer func() { _ = tasksResp.Body.Close() }()
	if tasksResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tasks status = %d", tasksResp.StatusCode)
	}
	var tasks []store.TaskRecord
	if err := json.NewDecoder(tasksResp.Body).Decode(&tasks); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if got := tasks[0].Status; got != store.TaskStatusPending {
		t.Fatalf("task status = %q, want %q", got, store.TaskStatusPending)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit")
	}
}

func TestCoordinatorAuditEndpointsRequireAuthWhenEnabled(t *testing.T) {
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "secret-token")

	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	seedCoordinatorAuditDB(t, dbPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: time.Second,
			dbPath:       dbPath,
			auth:         true,
		}, nil, ctx)
	}()
	waitForHealth(t, port)

	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := fmt.Sprintf("http://localhost:%d", port)

	for _, path := range []string{"/events?repo=owner/repo-a", "/tasks?repo=owner/repo-a"} {
		resp := getCoordinator(t, client, baseURL+path, "")
		if resp.StatusCode != http.StatusUnauthorized {
			_ = resp.Body.Close()
			t.Fatalf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusUnauthorized)
		}
		_ = resp.Body.Close()

		authResp := getCoordinator(t, client, baseURL+path, "secret-token")
		if authResp.StatusCode != http.StatusOK {
			_ = authResp.Body.Close()
			t.Fatalf("GET %s with auth status = %d, want %d", path, authResp.StatusCode, http.StatusOK)
		}
		_ = authResp.Body.Close()
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit")
	}
}

func TestCoordinatorCookieLoginEndToEnd(t *testing.T) {
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "secret-token")

	port := getFreePort(t)
	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	seedCoordinatorAuditDB(t, dbPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runCoordinatorWithOpts(&coordinatorOpts{
			port:         port,
			pollInterval: time.Second,
			dbPath:       dbPath,
			auth:         true,
			// Loopback test fixtures speak plain HTTP, so drop Secure on
			// the cookie or the test client would silently discard it
			// even though the server still sets it.
			cookieInsecure: true,
		}, nil, ctx)
	}()
	waitForHealth(t, port)

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Jar:     jar,
		// Don't auto-follow redirects so we can inspect the 302 from /login
		// and the auth-failure 302 from WrapAuth.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Browser visits a protected HTML route with no cookie -> 302 /login.
	htmlReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/status", nil)
	htmlReq.Header.Set("Accept", "text/html,application/xhtml+xml")
	htmlResp, err := client.Do(htmlReq)
	if err != nil {
		t.Fatalf("html GET: %v", err)
	}
	if htmlResp.StatusCode != http.StatusFound {
		t.Fatalf("html GET status = %d, want %d", htmlResp.StatusCode, http.StatusFound)
	}
	if loc := htmlResp.Header.Get("Location"); !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("html GET Location = %q, want /login?next=...", loc)
	}
	_ = htmlResp.Body.Close()

	// API client with no credentials still gets 401 JSON.
	apiResp, err := client.Get(baseURL + "/api/v1/status")
	if err != nil {
		t.Fatalf("api GET: %v", err)
	}
	if apiResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api GET status = %d, want %d", apiResp.StatusCode, http.StatusUnauthorized)
	}
	_ = apiResp.Body.Close()

	// POST /login with the right token -> 302 + Set-Cookie.
	form := url.Values{}
	form.Set("token", "secret-token")
	form.Set("next", "/api/v1/status")
	loginReq, _ := http.NewRequest(http.MethodPost, baseURL+"/login", strings.NewReader(form.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResp, err := client.Do(loginReq)
	if err != nil {
		t.Fatalf("login POST: %v", err)
	}
	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("login POST status = %d, want %d", loginResp.StatusCode, http.StatusFound)
	}
	if loc := loginResp.Header.Get("Location"); loc != "/api/v1/status" {
		t.Fatalf("login Location = %q, want /api/v1/status", loc)
	}
	cookies := loginResp.Cookies()
	if len(cookies) == 0 {
		t.Fatal("login POST returned no Set-Cookie")
	}
	var session *http.Cookie
	for _, c := range cookies {
		if c.Name == app.SessionCookieName {
			session = c
			break
		}
	}
	if session == nil {
		t.Fatalf("Set-Cookie missing %s", app.SessionCookieName)
	}
	if !session.HttpOnly || session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie attributes = %+v, want HttpOnly+SameSite=Strict", session)
	}
	_ = loginResp.Body.Close()

	// With the cookie now in the jar, the protected route returns 200.
	cookieResp, err := client.Get(baseURL + "/api/v1/status")
	if err != nil {
		t.Fatalf("cookie GET: %v", err)
	}
	if cookieResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(cookieResp.Body)
		_ = cookieResp.Body.Close()
		t.Fatalf("cookie GET status = %d, want %d (body=%s)", cookieResp.StatusCode, http.StatusOK, string(body))
	}
	_ = cookieResp.Body.Close()

	// Bearer header path is still alive (backwards compatibility).
	bearerReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/status", nil)
	bearerReq.Header.Set("Authorization", "Bearer secret-token")
	// Strip cookies so we know it's the bearer path that succeeded.
	bareClient := &http.Client{Timeout: 5 * time.Second}
	bearerResp, err := bareClient.Do(bearerReq)
	if err != nil {
		t.Fatalf("bearer GET: %v", err)
	}
	if bearerResp.StatusCode != http.StatusOK {
		t.Fatalf("bearer GET status = %d, want %d", bearerResp.StatusCode, http.StatusOK)
	}
	_ = bearerResp.Body.Close()

	// POST /logout clears the cookie.
	logoutReq, _ := http.NewRequest(http.MethodPost, baseURL+"/logout", nil)
	logoutResp, err := client.Do(logoutReq)
	if err != nil {
		t.Fatalf("logout POST: %v", err)
	}
	if logoutResp.StatusCode != http.StatusFound {
		t.Fatalf("logout status = %d, want %d", logoutResp.StatusCode, http.StatusFound)
	}
	cleared := false
	for _, c := range logoutResp.Cookies() {
		if c.Name == app.SessionCookieName && c.MaxAge == -1 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("logout did not clear %s (cookies=%+v)", app.SessionCookieName, logoutResp.Cookies())
	}
	_ = logoutResp.Body.Close()

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not exit")
	}
}

func TestCoordinatorWorkerSessionProxyUsesCoordinatorAuthSurface(t *testing.T) {
	var forwardedAuth []string
	mgmt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardedAuth = append(forwardedAuth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/sessions/session-123":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<a href="/sessions">Back</a><script>fetch("/sessions/session-123/events.json")</script>`))
		case "/sessions/session-123/events.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"session_id":"session-123"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mgmt.Close()

	dbPath := filepath.Join(t.TempDir(), "coordinator.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.InsertWorker(store.WorkerRecord{
		ID:          "worker-a",
		Repo:        "owner/repo",
		Roles:       `["dev"]`,
		Hostname:    "host-a",
		MgmtBaseURL: mgmt.URL,
		Status:      "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	api := &app.FullCoordinatorServer{Store: st, AuthEnabled: true, AuthToken: "secret-token"}
	mux := http.NewServeMux()
	mux.Handle("/workers/", api.WrapAuth(newCoordinatorSessionProxy(st, api.AuthToken)))
	server := httptest.NewServer(mux)
	defer server.Close()

	unauthResp, err := http.Get(server.URL + "/workers/worker-a/sessions/session-123")
	if err != nil {
		t.Fatalf("GET unauth proxy: %v", err)
	}
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, want %d", unauthResp.StatusCode, http.StatusUnauthorized)
	}
	_ = unauthResp.Body.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/workers/worker-a/sessions/session-123", nil)
	if err != nil {
		t.Fatalf("build req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET auth proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyText := string(body)
	if !strings.Contains(bodyText, `/workers/worker-a/sessions/session-123/events.json`) {
		t.Fatalf("proxy body missing rewritten session path: %s", bodyText)
	}
	if strings.Contains(bodyText, `"/sessions/session-123/events.json"`) {
		t.Fatalf("proxy body still contains raw worker-local session path: %s", bodyText)
	}
	if len(forwardedAuth) == 0 || forwardedAuth[len(forwardedAuth)-1] != "Bearer secret-token" {
		t.Fatalf("forwarded auth = %v, want Bearer secret-token", forwardedAuth)
	}
}
