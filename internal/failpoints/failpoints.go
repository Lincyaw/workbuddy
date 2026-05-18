// Package failpoints provides named, runtime-armable fault-injection points.
//
// In the default build (without the `faultinject` build tag) Trip and Hit are
// trivial no-op functions that return nil and that the compiler can inline
// away entirely. The build with `-tags faultinject` activates env-var arming
// at init plus the runtime Arm/Disarm API.
//
// Naming convention for hook sites: package.function.point, e.g.
//
//	poller.list_issues.before
//
// Production binaries MUST NOT be built with the `faultinject` tag.
package failpoints

import (
	"errors"
	"time"
)

// ErrFailpointReturn is the sentinel error returned by Hit when an armed
// effect of Kind=="return" trips. Callers that wish to short-circuit without
// surfacing a real error message can check for this with errors.Is.
var ErrFailpointReturn = errors.New("failpoint: return")

// Effect describes what an armed failpoint should do at trip time.
type Effect struct {
	// Kind is one of "error", "delay", "panic", "return".
	Kind string
	// Err is the human-readable error message for Kind=="error" and the
	// panic message for Kind=="panic".
	Err string
	// Delay is the sleep amount applied by Hit when Kind=="delay".
	Delay time.Duration
	// Once: when true, the first matching Trip removes the entry.
	Once bool
	// MatchRepo narrows the effect to callers that pass WithRepo with the
	// same value. Empty means match-all.
	MatchRepo string
	// MatchIssue narrows the effect to callers that pass WithIssue with the
	// same value. Zero means match-all.
	MatchIssue int
}

// matchCtx is built from MatchOpt values supplied at the trip site. It is
// only inspected by the faultinject build; the no-op build ignores it.
type matchCtx struct {
	repo  string
	issue int
}

// MatchOpt narrows a Trip/Hit call to a specific caller context.
type MatchOpt func(*matchCtx)

// WithRepo declares the calling repo for matching against Effect.MatchRepo.
func WithRepo(repo string) MatchOpt {
	return func(m *matchCtx) { m.repo = repo }
}

// WithIssue declares the calling issue number for matching against
// Effect.MatchIssue.
func WithIssue(num int) MatchOpt {
	return func(m *matchCtx) { m.issue = num }
}

// buildMatchCtx applies the options to a zero matchCtx. Kept package-private
// because both build variants want identical option-application semantics.
func buildMatchCtx(opts []MatchOpt) matchCtx {
	var m matchCtx
	for _, opt := range opts {
		if opt != nil {
			opt(&m)
		}
	}
	return m
}
