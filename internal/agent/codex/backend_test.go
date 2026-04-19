package codex

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/agent"
)

func TestCodexNewBackendMissingBinary(t *testing.T) {
	cfg := Config{Binary: "/nonexistent/codex-binary-that-does-not-exist"}
	_, err := NewBackend(cfg)
	if err == nil {
		t.Fatal("NewBackend with missing binary should return error")
	}
}

func TestCodexNewBackendDefaultBinary(t *testing.T) {
	// With default binary (codex), expect error since codex is not available in CI.
	cfg := Config{}
	b, err := NewBackend(cfg)
	if err != nil {
		// Expected: codex binary not found in PATH.
		t.Logf("NewBackend with default binary: %v (expected in test env)", err)
		return
	}
	// If codex IS available, verify the backend works.
	ctx := context.Background()
	defer func() { _ = b.Shutdown(ctx) }()

	var _ agent.Backend = b
}

func TestExtractThreadID(t *testing.T) {
	tests := []struct {
		name   string
		params string
		want   string
	}{
		{
			name:   "with thread_id",
			params: `{"thread_id":"abc-123","other":"data"}`,
			want:   "abc-123",
		},
		{
			name:   "empty thread_id",
			params: `{"thread_id":""}`,
			want:   "",
		},
		{
			name:   "no thread_id field",
			params: `{"other":"data"}`,
			want:   "",
		},
		{
			name:   "invalid json",
			params: `not json`,
			want:   "",
		},
		{
			name:   "null params",
			params: `null`,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractThreadID(json.RawMessage(tt.params))
			if got != tt.want {
				t.Fatalf("extractThreadID(%s) = %q, want %q", tt.params, got, tt.want)
			}
		})
	}
}

func TestCodexBackendInterfaceCompliance(t *testing.T) {
	// Compile-time check that Backend satisfies agent.Backend.
	var _ agent.Backend = (*Backend)(nil)
}

func TestCodexSessionInterfaceCompliance(t *testing.T) {
	// Compile-time check that session satisfies agent.Session.
	var _ agent.Session = (*session)(nil)
}
