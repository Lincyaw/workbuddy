package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPreflightCodexSidecar_DisabledIsNoop(t *testing.T) {
	got, err := preflightCodexSidecar(codexSidecarOpts{})
	if err != nil {
		t.Fatalf("disabled preflight: unexpected err: %v", err)
	}
	if got.binary != "" {
		t.Fatalf("disabled preflight: binary leaked: %q", got.binary)
	}
}

func TestPreflightCodexSidecar_RejectsMissingBinary(t *testing.T) {
	_, err := preflightCodexSidecar(codexSidecarOpts{binary: "/nonexistent/codex-binary-that-does-not-exist"})
	if err == nil {
		t.Fatal("expected error for missing binary, got nil")
	}
	if !strings.Contains(err.Error(), "not found or not executable") {
		t.Fatalf("error should name the failure mode; got: %v", err)
	}
}

func TestPreflightCodexSidecar_FillsDefaults(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := preflightCodexSidecar(codexSidecarOpts{binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	if got.listen != "127.0.0.1:7177" {
		t.Fatalf("default listen: got %q want 127.0.0.1:7177", got.listen)
	}
	if got.minBackoff != 1*time.Second || got.maxBackoff != 30*time.Second {
		t.Fatalf("default backoffs: min=%v max=%v", got.minBackoff, got.maxBackoff)
	}
}

func TestPreflightCodexSidecar_RejectsNonLoopback(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		"0.0.0.0:7177",
		"192.168.1.10:7177",
		"example.com:7177",
	}
	for _, listen := range cases {
		t.Run(listen, func(t *testing.T) {
			_, err := preflightCodexSidecar(codexSidecarOpts{binary: bin, listen: listen})
			if err == nil {
				t.Fatalf("non-loopback %q should be rejected", listen)
			}
			if !strings.Contains(err.Error(), "loopback") {
				t.Fatalf("error should mention loopback; got: %v", err)
			}
		})
	}
}

func TestPreflightCodexSidecar_RejectsMalformedListen(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := preflightCodexSidecar(codexSidecarOpts{binary: bin, listen: "not-a-host-port"})
	if err == nil {
		t.Fatal("malformed listen should be rejected")
	}
}

func TestPreflightCodexSidecar_AcceptsLoopbackVariants(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, listen := range []string{"127.0.0.1:7177", "localhost:7177", "[::1]:7177"} {
		t.Run(listen, func(t *testing.T) {
			if _, err := preflightCodexSidecar(codexSidecarOpts{binary: bin, listen: listen}); err != nil {
				t.Fatalf("loopback %q should be accepted: %v", listen, err)
			}
		})
	}
}

func TestRunCodexSidecar_DisabledReturnsImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := runCodexSidecar(ctx, codexSidecarOpts{}); err != nil {
		t.Fatalf("disabled sidecar: %v", err)
	}
}

func TestRunCodexSidecar_StartsAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	// Build a fake codex that just sleeps so we can observe the
	// supervised lifecycle is alive without a real codex binary.
	fake := filepath.Join(dir, "fake-codex")
	script := `#!/bin/sh
trap 'exit 0' TERM INT
while sleep 60; do :; done
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	opts, err := preflightCodexSidecar(codexSidecarOpts{
		binary:     fake,
		listen:     "127.0.0.1:0", // host:port shape; child won't actually bind, but our test stops it before that matters
		minBackoff: 50 * time.Millisecond,
		maxBackoff: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runCodexSidecar(ctx, opts) }()

	// Give the loop time to spawn the fake.
	time.Sleep(200 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sidecar shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sidecar did not shut down within 5s of cancel")
	}
}

func TestRunCodexSidecar_RestartsOnCrash(t *testing.T) {
	dir := t.TempDir()
	// Fake exits immediately; we want to see the loop relaunch it
	// at least twice within a short window before we cancel.
	counter := filepath.Join(dir, "starts")
	fake := filepath.Join(dir, "fake-codex")
	script := `#!/bin/sh
echo "start" >> "` + counter + `"
exit 1
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	opts, err := preflightCodexSidecar(codexSidecarOpts{
		binary:     fake,
		listen:     "127.0.0.1:0",
		minBackoff: 30 * time.Millisecond,
		maxBackoff: 60 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = runCodexSidecar(ctx, opts)

	data, _ := os.ReadFile(counter)
	starts := strings.Count(string(data), "start")
	if starts < 2 {
		t.Fatalf("expected at least 2 restarts inside 600ms; got %d. counter=%q", starts, string(data))
	}
}

func TestNextBackoff(t *testing.T) {
	if got := nextBackoff(1*time.Second, 30*time.Second); got != 2*time.Second {
		t.Fatalf("1s -> %v want 2s", got)
	}
	if got := nextBackoff(20*time.Second, 30*time.Second); got != 30*time.Second {
		t.Fatalf("20s -> %v want 30s", got)
	}
	if got := nextBackoff(100*time.Second, 30*time.Second); got != 30*time.Second {
		t.Fatalf("100s -> %v want 30s (capped)", got)
	}
}

func TestDefaultCodexSidecarURL(t *testing.T) {
	if got := defaultCodexSidecarURL(); got != "ws://127.0.0.1:7177" {
		t.Fatalf("default url: got %q", got)
	}
}

func TestPrefixedWriter_AddsPrefixPerLine(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "log-*")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	w := newPrefixedWriter(tmp, "[X] ")
	if _, err := w.Write([]byte("line1\nline2\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(" tail\n")); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	want := "[X] line1\n[X] line2\n[X] partial tail\n"
	if got != want {
		t.Fatalf("prefix output mismatch.\n got: %q\nwant: %q", got, want)
	}
}
