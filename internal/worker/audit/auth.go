package audit

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerAuth wraps `next`, requiring an `Authorization: Bearer <token>`
// header where the token matches the configured value. Mismatches return
// 401 with a small JSON body. The token comparison is constant-time so a
// timing attacker cannot probe the token byte-by-byte.
//
// An empty configured token is treated as a programming error and rejects
// every request — Start refuses to construct a Server with an empty
// token, so reaching this with token="" only happens in tests that
// bypass Start, and the safer default is fail-closed.
func bearerAuth(token string, next http.Handler) http.Handler {
	expected := []byte(strings.TrimSpace(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(expected) == 0 {
			writeUnauthorized(w, "audit token not configured")
			return
		}
		got := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(got, "Bearer ") {
			writeUnauthorized(w, "missing bearer token")
			return
		}
		provided := []byte(strings.TrimSpace(strings.TrimPrefix(got, "Bearer ")))
		if subtle.ConstantTimeCompare(expected, provided) != 1 {
			writeUnauthorized(w, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}
