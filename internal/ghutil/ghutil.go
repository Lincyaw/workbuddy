// Package ghutil provides shared GitHub/gh CLI helpers.
package ghutil

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	// Default remaining API request threshold used for startup warnings.
	defaultRateLimitWarnThreshold = 100
)

var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ghp_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`(?i)ghs_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`(?i)github_pat_[A-Za-z0-9_]{20,}`),
}

// IsRateLimit returns true when an error message explicitly mentions a rate limit.
//
// This intentionally requires textual "rate limit" content and does not classify
// bare 403 responses as rate-limit errors.
func IsRateLimit(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rate limit")
}

// RedactTokens removes common GitHub token patterns from logs/output text.
func RedactTokens(raw string) string {
	out := raw
	for _, re := range tokenPatterns {
		out = re.ReplaceAllString(out, "[REDACTED]")
	}
	return out
}

// RateLimitBudget represents the primary REST API budget data returned by gh api rate_limit.
type RateLimitBudget struct {
	Limit     int
	Remaining int
	Used      int
	ResetAt   time.Time
}

// CurrentRateLimitBudget queries `gh api rate_limit` and extracts core token budget.
//
// On failure it returns a wrapped error that is safe to propagate or log.
func CurrentRateLimitBudget(ctx context.Context) (RateLimitBudget, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "rate_limit")
	out, err := cmd.Output()
	if err != nil {
		return RateLimitBudget{}, fmt.Errorf("gh api rate_limit: %w", err)
	}

	var payload struct {
		Resources struct {
			Core struct {
				Limit     int `json:"limit"`
				Remaining int `json:"remaining"`
				Used      int `json:"used"`
				Reset     int `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return RateLimitBudget{}, fmt.Errorf("gh api rate_limit: parse JSON: %w", err)
	}

	resetAt := time.Unix(int64(payload.Resources.Core.Reset), 0).UTC()
	if payload.Resources.Core.Reset == 0 {
		resetAt = time.Time{}
	}
	return RateLimitBudget{
		Limit:     payload.Resources.Core.Limit,
		Remaining: payload.Resources.Core.Remaining,
		Used:      payload.Resources.Core.Used,
		ResetAt:   resetAt,
	}, nil
}

// WarnIfLowBudget logs a warning when remaining quota is below threshold.
//
// The check is best-effort and logs/returns warnings only; callers should keep
// service startup moving even when the check cannot run.
func WarnIfLowBudget(ctx context.Context, threshold int, logger *log.Logger) (RateLimitBudget, bool, error) {
	budget, err := CurrentRateLimitBudget(ctx)
	if err != nil {
		if logger != nil {
			logger.Printf("[ghutil] rate limit budget check failed: %v", RedactTokens(err.Error()))
		}
		return budget, false, err
	}
	if budget.Remaining < threshold {
		if logger != nil {
			when := "unknown"
			if !budget.ResetAt.IsZero() {
				when = budget.ResetAt.Format(time.RFC3339)
			}
			logger.Printf("[ghutil] warning: remaining GitHub rate limit for core requests is low (%d < %d), reset at %s", budget.Remaining, threshold, when)
		}
		return budget, true, nil
	}
	return budget, false, nil
}

// WarnIfLowBudgetDefault logs using the package default threshold and the standard log package.
func WarnIfLowBudgetDefault(ctx context.Context) (RateLimitBudget, bool, error) {
	return WarnIfLowBudget(ctx, defaultRateLimitWarnThreshold, log.Default())
}
