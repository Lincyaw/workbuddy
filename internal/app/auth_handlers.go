package app

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

// loginPageTemplate is a minimal inline form. Inputs are auto-escaped by
// html/template; `next` round-trips back into the form so a successful
// login can redirect to the original target.
var loginPageTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>workbuddy login</title>
<style>
body{font-family:system-ui,sans-serif;max-width:24rem;margin:4rem auto;padding:0 1rem}
h1{font-size:1.25rem;margin:0 0 1rem}
label{display:block;margin:.5rem 0 .25rem}
input[type=password]{width:100%;padding:.5rem;font:inherit;box-sizing:border-box}
button{margin-top:1rem;padding:.5rem 1rem;font:inherit;cursor:pointer}
.error{color:#b00;margin:.5rem 0;font-size:.9rem}
</style>
</head>
<body>
<h1>workbuddy login</h1>
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<form method="post" action="/login">
  <label for="token">Token</label>
  <input id="token" name="token" type="password" autocomplete="off" autofocus required>
  <input type="hidden" name="next" value="{{.Next}}">
  <button type="submit">Sign in</button>
</form>
</body>
</html>
`))

// HandleLogin serves GET /login (form) and POST /login (token submission).
//
// On POST: reads the form `token` field, validates it via isAuthorizedBearer,
// and on success sets the wb_session cookie and 302s to `next` (or `/`).
// On failure the form is re-rendered with HTTP 200 plus an error message.
func (s *FullCoordinatorServer) HandleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderLoginPage(w, http.StatusOK, sanitizeNext(r.URL.Query().Get("next")), "")
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			s.renderLoginPage(w, http.StatusBadRequest, "/", "Could not parse form")
			return
		}
		token := strings.TrimSpace(r.PostForm.Get("token"))
		next := sanitizeNext(r.PostForm.Get("next"))
		if !s.isAuthorizedBearer(token) {
			// Re-render the form with an error. Status stays 200 so the
			// browser keeps the URL stable; an explicit error message tells
			// the user what happened.
			s.renderLoginPage(w, http.StatusOK, next, "Invalid token")
			return
		}
		http.SetCookie(w, s.buildSessionCookie(token, SessionCookieMaxAge))
		http.Redirect(w, r, next, http.StatusFound)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleLogout serves POST /logout: clears the wb_session cookie and 302s
// the browser to /login.
func (s *FullCoordinatorServer) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, s.buildSessionCookie("", -1))
	http.Redirect(w, r, "/login", http.StatusFound)
}

// buildSessionCookie composes the wb_session cookie. maxAge < 0 expires the
// cookie immediately (logout); a positive maxAge sets the lifetime in
// seconds. Secure is forced unless CookieInsecure was explicitly set.
func (s *FullCoordinatorServer) buildSessionCookie(value string, maxAge int) *http.Cookie {
	c := &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   !s.CookieInsecure,
		MaxAge:   maxAge,
	}
	return c
}

// renderLoginPage writes the login form with the given status code, next
// target, and optional error string. Falls back to plain text if the
// template fails — the HTML is small enough that this should not happen
// in practice, but we don't want a panic to leak.
func (s *FullCoordinatorServer) renderLoginPage(w http.ResponseWriter, status int, next, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = loginPageTemplate.Execute(w, struct {
		Next  string
		Error string
	}{
		Next:  next,
		Error: errMsg,
	})
}

// sanitizeNext keeps redirect targets local to this host: only same-origin
// paths (starting with `/` and not `//`) are accepted; anything else falls
// back to `/`. This blocks open-redirect attempts via ?next=https://evil/.
func sanitizeNext(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "/"
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		return "/"
	}
	return raw
}
