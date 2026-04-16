package ghutil

import (
	"strings"
	"testing"
)

func TestIsRateLimit(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "403 without rate limit token",
			err:  errTest("HTTP 403: must have permission to perform this action"),
			want: false,
		},
		{
			name: "403 with rate limit text",
			err:  errTest("HTTP 403: rate limit exceeded"),
			want: true,
		},
		{
			name: "403 with uppercase rate limit text",
			err:  errTest("Error: RATE LIMIT exceeded"),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsRateLimit(tc.err)
			if got != tc.want {
				t.Fatalf("IsRateLimit(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRedactTokens(t *testing.T) {
	raw := "error ghp_12345678901234567890 ghs_abcdefghijklmnopqrstuvwxyz0123 github_pat_ABCDEF12345678901234"
	redacted := RedactTokens(raw)
	if strings.Contains(redacted, "ghp_") || strings.Contains(redacted, "ghs_") || strings.Contains(redacted, "github_pat_") {
		t.Fatalf("tokens were not redacted: %q", redacted)
	}
	if strings.Count(redacted, "[REDACTED]") != 3 {
		t.Fatalf("expected 3 redactions, got %q", redacted)
	}
}

type errTest string

func (e errTest) Error() string {
	return string(e)
}
