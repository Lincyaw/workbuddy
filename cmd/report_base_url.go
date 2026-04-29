package cmd

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// resolveReportBaseURL validates and normalizes the --report-base-url flag for
// commands whose Reporter writes session links into GitHub issue comments.
//
// Rules:
//   - When listenAddr is loopback, an empty flag falls back to the env var, and
//     if both are empty, defaults to "http://<listenAddr>" so single-host dev
//     workflows keep working.
//   - When listenAddr is non-loopback, the flag (or env var) MUST be set and
//     MUST resolve to a non-loopback host; otherwise session links posted into
//     issue comments would be unclickable from a browser.
//
// component is the CLI surface ("coordinator", "serve") used in error
// messages. envValue is the WORKBUDDY_REPORT_BASE_URL fallback.
func resolveReportBaseURL(component, listenAddr, flagValue, envValue string) (string, error) {
	flagValue = strings.TrimSpace(flagValue)
	envValue = strings.TrimSpace(envValue)
	listenIsLoopback := isLoopbackListenAddr(listenAddr)

	picked := flagValue
	if picked == "" {
		picked = envValue
	}
	picked = strings.TrimRight(picked, "/")

	if listenIsLoopback {
		if picked == "" {
			return "http://" + listenAddr, nil
		}
		if err := validateReportBaseURLString(component, picked); err != nil {
			return "", err
		}
		return picked, nil
	}

	// Non-loopback bind path.
	if picked == "" {
		return "", fmt.Errorf(
			"%[1]s: --listen %[2]s is non-loopback but --report-base-url is missing.\n"+
				"Session links in GitHub comments would be unclickable from a browser.\n"+
				"Pass --report-base-url=http://<your-host>:<port> (or set WORKBUDDY_REPORT_BASE_URL env var) to fix.",
			component, listenAddr,
		)
	}
	if err := validateReportBaseURLString(component, picked); err != nil {
		return "", err
	}
	if isLoopbackReportBaseURL(picked) {
		return "", fmt.Errorf(
			"%[1]s: --listen %[2]s is non-loopback but --report-base-url is loopback (%[3]s).\n"+
				"Session links in GitHub comments would be unclickable from a browser.\n"+
				"Pass --report-base-url=http://<your-host>:<port> (or set WORKBUDDY_REPORT_BASE_URL env var) to fix.",
			component, listenAddr, picked,
		)
	}
	return picked, nil
}

func validateReportBaseURLString(component, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid --report-base-url %q: %w", component, raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s: --report-base-url must use http or https, got %q", component, raw)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return fmt.Errorf("%s: --report-base-url must include a host, got %q", component, raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s: --report-base-url must not include query or fragment, got %q", component, raw)
	}
	return nil
}

func isLoopbackReportBaseURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
