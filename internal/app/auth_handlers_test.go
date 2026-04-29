package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeProtected is a tiny next.ServeHTTP that returns 204 so a successful
// WrapAuth call shows up clearly in test assertions.
func fakeProtected() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestWrapAuthAllowsCookieAndHeaderAndRejectsBoth(t *testing.T) {
	server := &FullCoordinatorServer{
		AuthEnabled: true,
		AuthToken:   "shared-secret",
	}
	protected := server.WrapAuth(fakeProtected())

	t.Run("bearer header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		req.Header.Set("Authorization", "Bearer shared-secret")
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
		}
	})

	t.Run("session cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "shared-secret"})
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
		}
	})

	t.Run("no credential, json client", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if !strings.Contains(rec.Body.String(), "unauthorized") {
			t.Fatalf("body = %q, want unauthorized JSON", rec.Body.String())
		}
	})

	t.Run("no credential, html client redirects to login", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sessions/abc", nil)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
		}
		loc := rec.Header().Get("Location")
		want := "/login?next=" + url.QueryEscape("/sessions/abc")
		if loc != want {
			t.Fatalf("Location = %q, want %q", loc, want)
		}
	})

	t.Run("bad cookie value falls through to 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "wrong-secret"})
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
}

func TestWrapAuthDisabledShortCircuits(t *testing.T) {
	server := &FullCoordinatorServer{AuthEnabled: false}
	protected := server.WrapAuth(fakeProtected())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestHandleLoginPostSuccessSetsSecureCookie(t *testing.T) {
	server := &FullCoordinatorServer{
		AuthEnabled: true,
		AuthToken:   "shared-secret",
	}

	form := url.Values{}
	form.Set("token", "shared-secret")
	form.Set("next", "/sessions/abc")
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	server.HandleLogin(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/sessions/abc" {
		t.Fatalf("Location = %q, want /sessions/abc", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected Set-Cookie header on successful login")
	}
	c := cookies[0]
	if c.Name != SessionCookieName {
		t.Fatalf("cookie name = %q, want %q", c.Name, SessionCookieName)
	}
	if c.Value != "shared-secret" {
		t.Fatalf("cookie value = %q, want shared-secret", c.Value)
	}
	if !c.HttpOnly {
		t.Error("expected HttpOnly cookie")
	}
	if !c.Secure {
		t.Error("expected Secure cookie when CookieInsecure is false")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	if c.MaxAge != SessionCookieMaxAge {
		t.Errorf("MaxAge = %d, want %d", c.MaxAge, SessionCookieMaxAge)
	}
}

func TestHandleLoginPostFailureReRendersForm(t *testing.T) {
	server := &FullCoordinatorServer{
		AuthEnabled: true,
		AuthToken:   "shared-secret",
	}

	form := url.Values{}
	form.Set("token", "wrong-secret")
	form.Set("next", "/sessions/abc")
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	server.HandleLogin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if cookies := rec.Result().Cookies(); len(cookies) > 0 {
		t.Fatalf("expected no Set-Cookie on failed login, got %+v", cookies)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Invalid token") {
		t.Errorf("body should contain error message, got %q", body)
	}
	// next must round-trip in the form so the user retries with the original
	// destination intact.
	if !strings.Contains(body, `value="/sessions/abc"`) {
		t.Errorf("body should preserve next=/sessions/abc, got %q", body)
	}
}

func TestHandleLoginGetRendersForm(t *testing.T) {
	server := &FullCoordinatorServer{AuthEnabled: true, AuthToken: "shared-secret"}
	req := httptest.NewRequest(http.MethodGet, "/login?next=/sessions/abc", nil)
	rec := httptest.NewRecorder()

	server.HandleLogin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), `value="/sessions/abc"`) {
		t.Errorf("expected next round-trip in form body")
	}
}

func TestHandleLogoutClearsCookieAndRedirects(t *testing.T) {
	server := &FullCoordinatorServer{AuthEnabled: true, AuthToken: "shared-secret"}
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	rec := httptest.NewRecorder()

	server.HandleLogout(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("Location = %q, want /login", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected Set-Cookie clearing the session")
	}
	c := cookies[0]
	if c.Name != SessionCookieName {
		t.Fatalf("cookie name = %q, want %q", c.Name, SessionCookieName)
	}
	if c.MaxAge != -1 {
		t.Errorf("MaxAge = %d, want -1 (immediate expiry)", c.MaxAge)
	}
}

func TestCookieInsecureDropsSecureAttribute(t *testing.T) {
	server := &FullCoordinatorServer{
		AuthEnabled:    true,
		AuthToken:      "shared-secret",
		CookieInsecure: true,
	}

	form := url.Values{}
	form.Set("token", "shared-secret")
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	server.HandleLogin(rec, req)

	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected Set-Cookie")
	}
	if cookies[0].Secure {
		t.Errorf("expected Secure to be false when CookieInsecure is true")
	}
	if !cookies[0].HttpOnly {
		t.Errorf("HttpOnly should still be set")
	}
	if cookies[0].SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite should still be Strict, got %v", cookies[0].SameSite)
	}
}

func TestSanitizeNextRejectsExternalRedirects(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "/"},
		{"/sessions/abc", "/sessions/abc"},
		{"/sessions/abc?foo=bar", "/sessions/abc?foo=bar"},
		{"//evil.example.com/foo", "/"},
		{"https://evil.example.com/foo", "/"},
		{"javascript:alert(1)", "/"},
		{"  /spaced  ", "/spaced"},
	}
	for _, tc := range cases {
		got := sanitizeNext(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeNext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHandleLoginRejectsNonGetPost(t *testing.T) {
	server := &FullCoordinatorServer{AuthEnabled: true, AuthToken: "shared-secret"}
	req := httptest.NewRequest(http.MethodDelete, "/login", nil)
	rec := httptest.NewRecorder()
	server.HandleLogin(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleLogoutRequiresPost(t *testing.T) {
	server := &FullCoordinatorServer{AuthEnabled: true, AuthToken: "shared-secret"}
	req := httptest.NewRequest(http.MethodGet, "/logout", nil)
	rec := httptest.NewRecorder()
	server.HandleLogout(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
