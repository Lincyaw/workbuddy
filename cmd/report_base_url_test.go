package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestResolveReportBaseURL_LoopbackDefault(t *testing.T) {
	got, err := resolveReportBaseURL("coordinator", "127.0.0.1:8081", "", "")
	if err != nil {
		t.Fatalf("loopback default rejected: %v", err)
	}
	if want := "http://127.0.0.1:8081"; got != want {
		t.Fatalf("got = %q, want %q", got, want)
	}
}

func TestResolveReportBaseURL_LoopbackOverride(t *testing.T) {
	got, err := resolveReportBaseURL("coordinator", "127.0.0.1:8081", "https://reverse-proxy.example.com:8081/", "")
	if err != nil {
		t.Fatalf("loopback override rejected: %v", err)
	}
	if want := "https://reverse-proxy.example.com:8081"; got != want {
		t.Fatalf("got = %q, want %q (slashes should be trimmed)", got, want)
	}
}

func TestResolveReportBaseURL_NonLoopbackRequiresFlag(t *testing.T) {
	_, err := resolveReportBaseURL("coordinator", "0.0.0.0:8081", "", "")
	if err == nil {
		t.Fatal("expected error for non-loopback bind without --report-base-url")
	}
	for _, frag := range []string{
		"--listen 0.0.0.0:8081 is non-loopback",
		"--report-base-url is missing",
		"--report-base-url=http://<your-host>:<port>",
		"WORKBUDDY_REPORT_BASE_URL",
	} {
		if !strings.Contains(err.Error(), frag) {
			t.Fatalf("error %q missing fragment %q", err.Error(), frag)
		}
	}
}

func TestResolveReportBaseURL_NonLoopbackRejectsLoopbackURL(t *testing.T) {
	_, err := resolveReportBaseURL("coordinator", "0.0.0.0:8081", "http://127.0.0.1:8081", "")
	if err == nil {
		t.Fatal("expected error for non-loopback bind with loopback --report-base-url")
	}
	if !strings.Contains(err.Error(), "is loopback") {
		t.Fatalf("error = %q, want loopback diagnostic", err.Error())
	}
	if !strings.Contains(err.Error(), "Pass --report-base-url=http://<your-host>:<port>") {
		t.Fatalf("error = %q, want fix-it suggestion", err.Error())
	}
}

func TestResolveReportBaseURL_NonLoopbackAcceptsExternalHost(t *testing.T) {
	got, err := resolveReportBaseURL("coordinator", "0.0.0.0:8081", "http://example.com:8081", "")
	if err != nil {
		t.Fatalf("non-loopback bind with external URL rejected: %v", err)
	}
	if want := "http://example.com:8081"; got != want {
		t.Fatalf("got = %q, want %q", got, want)
	}
}

func TestResolveReportBaseURL_FallsBackToEnvVar(t *testing.T) {
	got, err := resolveReportBaseURL("coordinator", "0.0.0.0:8081", "", "https://env-host.example.com:8081")
	if err != nil {
		t.Fatalf("env-fallback rejected: %v", err)
	}
	if want := "https://env-host.example.com:8081"; got != want {
		t.Fatalf("got = %q, want %q", got, want)
	}
}

func TestResolveReportBaseURL_RejectsBadScheme(t *testing.T) {
	_, err := resolveReportBaseURL("coordinator", "0.0.0.0:8081", "ftp://example.com", "")
	if err == nil || !strings.Contains(err.Error(), "must use http or https") {
		t.Fatalf("expected scheme rejection, got %v", err)
	}
}

func newCoordinatorFlagCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "coordinator"}
	cmd.Flags().String("db", ".workbuddy/workbuddy.db", "")
	cmd.Flags().String("listen", "127.0.0.1:8081", "")
	cmd.Flags().Bool("loopback-only", false, "")
	cmd.Flags().String("config-dir", "", "")
	cmd.Flags().Duration("poll-interval", defaultPollInterval, "")
	cmd.Flags().Int("port", 8081, "")
	cmd.Flags().Bool("auth", false, "")
	cmd.Flags().String("trusted-authors", "", "")
	cmd.Flags().String("report-base-url", "", "")
	return cmd
}

func TestCoordinatorRejectsNonLoopbackWithoutBaseURL_Flag(t *testing.T) {
	t.Setenv("WORKBUDDY_REPORT_BASE_URL", "")
	cmd := newCoordinatorFlagCommand()
	if err := cmd.Flags().Set("listen", "0.0.0.0:8081"); err != nil {
		t.Fatalf("set listen: %v", err)
	}
	if err := cmd.Flags().Set("auth", "true"); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	_, err := parseCoordinatorFlags(cmd)
	if err == nil {
		t.Fatal("expected error for non-loopback bind without --report-base-url")
	}
	if !strings.Contains(err.Error(), "--report-base-url is missing") {
		t.Fatalf("error = %q, want --report-base-url-missing diagnostic", err.Error())
	}
}

func TestCoordinatorRejectsLoopbackBaseURLForNonLoopbackBind(t *testing.T) {
	t.Setenv("WORKBUDDY_REPORT_BASE_URL", "")
	cmd := newCoordinatorFlagCommand()
	if err := cmd.Flags().Set("listen", "0.0.0.0:8081"); err != nil {
		t.Fatalf("set listen: %v", err)
	}
	if err := cmd.Flags().Set("auth", "true"); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	if err := cmd.Flags().Set("report-base-url", "http://127.0.0.1:8081"); err != nil {
		t.Fatalf("set report-base-url: %v", err)
	}
	_, err := parseCoordinatorFlags(cmd)
	if err == nil {
		t.Fatal("expected error for loopback --report-base-url when bind is non-loopback")
	}
	if !strings.Contains(err.Error(), "is loopback") {
		t.Fatalf("error = %q, want loopback diagnostic", err.Error())
	}
}

func TestCoordinatorAllowsNonLoopbackWithExternalBaseURL(t *testing.T) {
	t.Setenv("WORKBUDDY_REPORT_BASE_URL", "")
	cmd := newCoordinatorFlagCommand()
	if err := cmd.Flags().Set("listen", "0.0.0.0:8081"); err != nil {
		t.Fatalf("set listen: %v", err)
	}
	if err := cmd.Flags().Set("auth", "true"); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	if err := cmd.Flags().Set("report-base-url", "http://example.com:8081"); err != nil {
		t.Fatalf("set report-base-url: %v", err)
	}
	opts, err := parseCoordinatorFlags(cmd)
	if err != nil {
		t.Fatalf("parseCoordinatorFlags: %v", err)
	}
	if want := "http://example.com:8081"; opts.reportBaseURL != want {
		t.Fatalf("reportBaseURL = %q, want %q", opts.reportBaseURL, want)
	}
}

func TestCoordinatorAllowsLoopbackWithDefaults(t *testing.T) {
	t.Setenv("WORKBUDDY_REPORT_BASE_URL", "")
	cmd := newCoordinatorFlagCommand()
	if err := cmd.Flags().Set("listen", "127.0.0.1:8081"); err != nil {
		t.Fatalf("set listen: %v", err)
	}
	opts, err := parseCoordinatorFlags(cmd)
	if err != nil {
		t.Fatalf("parseCoordinatorFlags: %v", err)
	}
	if want := "http://127.0.0.1:8081"; opts.reportBaseURL != want {
		t.Fatalf("reportBaseURL = %q, want default %q", opts.reportBaseURL, want)
	}
}

func TestCoordinatorReportBaseURLFromEnvVar(t *testing.T) {
	t.Setenv("WORKBUDDY_REPORT_BASE_URL", "https://env.example.com:8081")
	cmd := newCoordinatorFlagCommand()
	if err := cmd.Flags().Set("listen", "0.0.0.0:8081"); err != nil {
		t.Fatalf("set listen: %v", err)
	}
	if err := cmd.Flags().Set("auth", "true"); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	opts, err := parseCoordinatorFlags(cmd)
	if err != nil {
		t.Fatalf("parseCoordinatorFlags: %v", err)
	}
	if want := "https://env.example.com:8081"; opts.reportBaseURL != want {
		t.Fatalf("reportBaseURL = %q, want %q", opts.reportBaseURL, want)
	}
}
