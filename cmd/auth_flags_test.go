package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCoordinatorAuthTokenValue_EnvFallback(t *testing.T) {
	t.Setenv(coordinatorAuthTokenEnvVar, "env-token")
	got, err := resolveCoordinatorAuthTokenValue("")
	if err != nil {
		t.Fatalf("resolveCoordinatorAuthTokenValue: %v", err)
	}
	if got != "env-token" {
		t.Fatalf("token = %q, want %q", got, "env-token")
	}
}

func TestResolveCoordinatorAuthTokenValue_EmptyTokenFile(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(tokenPath, []byte(" \n"), 0o644); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	_, err := resolveCoordinatorAuthTokenValue(tokenPath)
	if err == nil {
		t.Fatal("expected empty token file error")
	}
	if !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}
