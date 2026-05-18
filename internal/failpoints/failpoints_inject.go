//go:build faultinject

package failpoints

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// envVar is the environment variable parsed at package init time.
const envVar = "WORKBUDDY_FAILPOINTS"

// registry stores armed effects keyed by failpoint name.
// We use a plain map under a RWMutex rather than sync.Map because Once
// requires a read-then-delete atomic step that is cleaner with explicit
// locking, and the registry is tiny (one entry per instrumented call site).
var (
	mu       sync.RWMutex
	registry = map[string]*Effect{}
)

// Trip looks up the named failpoint. If armed and the optional match
// predicates accept the caller, it returns a copy of the effect (and removes
// the entry when Effect.Once is set). Otherwise it returns nil.
func Trip(name string, opts ...MatchOpt) *Effect {
	mu.RLock()
	eff, ok := registry[name]
	mu.RUnlock()
	if !ok {
		return nil
	}
	mctx := buildMatchCtx(opts)
	if eff.MatchRepo != "" && eff.MatchRepo != mctx.repo {
		return nil
	}
	if eff.MatchIssue != 0 && eff.MatchIssue != mctx.issue {
		return nil
	}
	if eff.Once {
		mu.Lock()
		// Re-check under write lock in case another goroutine already
		// consumed the one-shot.
		cur, stillThere := registry[name]
		if !stillThere || cur != eff {
			mu.Unlock()
			return nil
		}
		delete(registry, name)
		mu.Unlock()
	}
	// Return a copy so the caller cannot mutate the registry entry.
	out := *eff
	return &out
}

// Hit looks up the named failpoint and applies its effect:
//   - error: returns errors.New(eff.Err)
//   - delay: sleeps eff.Delay and returns nil
//   - panic: panics with eff.Err
//   - return: returns ErrFailpointReturn
//
// Returns nil when no effect is armed (or the match predicates filter the
// caller out).
func Hit(name string, opts ...MatchOpt) error {
	eff := Trip(name, opts...)
	if eff == nil {
		return nil
	}
	switch eff.Kind {
	case "error":
		msg := eff.Err
		if msg == "" {
			msg = "failpoint: " + name
		}
		return errors.New(msg)
	case "delay":
		if eff.Delay > 0 {
			time.Sleep(eff.Delay)
		}
		return nil
	case "panic":
		msg := eff.Err
		if msg == "" {
			msg = "failpoint: " + name
		}
		panic(msg)
	case "return":
		return ErrFailpointReturn
	default:
		// Unknown kind: surface as a plain error so callers notice during
		// fault-injection runs but production cannot end up here (this
		// file only compiles under -tags faultinject).
		return fmt.Errorf("failpoint %q: unknown kind %q", name, eff.Kind)
	}
}

// Arm installs eff for name. Returns true when this call newly armed the
// failpoint (i.e. there was no previous entry under that name).
func Arm(name string, eff Effect) bool {
	mu.Lock()
	defer mu.Unlock()
	_, existed := registry[name]
	// Defensive copy so the caller mutating eff after Arm doesn't race us.
	stored := eff
	registry[name] = &stored
	return !existed
}

// Disarm removes the entry for name if present. No error when absent.
func Disarm(name string) {
	mu.Lock()
	delete(registry, name)
	mu.Unlock()
}

// Reset removes every armed effect. Mainly used by tests.
func Reset() {
	mu.Lock()
	registry = map[string]*Effect{}
	mu.Unlock()
}

// Enabled reports true under the faultinject build tag.
func Enabled() bool { return true }

func init() {
	raw := os.Getenv(envVar)
	if raw == "" {
		return
	}
	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, eff, err := parseEntry(entry)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failpoints: ignoring %q in %s: %v\n", entry, envVar, err)
			continue
		}
		Arm(name, eff)
	}
}

// parseEntry parses a single `name=kind(args...)` clause. The grammar is
// permissive: unknown comma-separated tokens inside the parens are reported
// to stderr but do not abort the parse.
func parseEntry(s string) (string, Effect, error) {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return "", Effect{}, fmt.Errorf("missing '=' separator")
	}
	name := strings.TrimSpace(s[:eq])
	rest := strings.TrimSpace(s[eq+1:])
	if name == "" {
		return "", Effect{}, fmt.Errorf("empty name")
	}

	// rest is `kind` or `kind(args)`.
	var kind, argStr string
	if open := strings.IndexByte(rest, '('); open >= 0 {
		if !strings.HasSuffix(rest, ")") {
			return "", Effect{}, fmt.Errorf("missing closing ')'")
		}
		kind = strings.TrimSpace(rest[:open])
		argStr = rest[open+1 : len(rest)-1]
	} else {
		kind = rest
	}

	eff := Effect{Kind: kind}
	args := splitArgs(argStr)

	// Per-kind primary argument handling. We treat the first positional
	// token as the message/duration when present; subsequent tokens are
	// suffix flags (once, repo=..., issue=...) regardless of kind.
	var positional []string
	var suffix []string
	for _, a := range args {
		if isSuffixToken(a) {
			suffix = append(suffix, a)
		} else {
			positional = append(positional, a)
		}
	}

	switch kind {
	case "error":
		if len(positional) > 0 {
			eff.Err = positional[0]
		}
	case "delay":
		if len(positional) == 0 {
			return "", Effect{}, fmt.Errorf("delay requires a duration argument")
		}
		d, derr := time.ParseDuration(positional[0])
		if derr != nil {
			return "", Effect{}, fmt.Errorf("invalid delay duration %q: %w", positional[0], derr)
		}
		eff.Delay = d
	case "panic":
		if len(positional) > 0 {
			eff.Err = positional[0]
		}
	case "return":
		// no positional args expected; ignore extras silently
	default:
		return "", Effect{}, fmt.Errorf("unknown kind %q", kind)
	}

	// Any positional tokens after the first are unexpected for the kinds
	// we recognise. Warn but keep going.
	if len(positional) > 1 {
		fmt.Fprintf(os.Stderr, "failpoints: extra positional args %v in %q ignored\n", positional[1:], s)
	}

	for _, tok := range suffix {
		if err := applySuffix(&eff, tok); err != nil {
			fmt.Fprintf(os.Stderr, "failpoints: bad suffix %q in %q: %v\n", tok, s, err)
		}
	}

	return name, eff, nil
}

// isSuffixToken returns true when arg is one of the recognised suffix flags
// (once, repo=..., issue=...).
func isSuffixToken(arg string) bool {
	if arg == "once" {
		return true
	}
	if strings.HasPrefix(arg, "repo=") || strings.HasPrefix(arg, "issue=") {
		return true
	}
	return false
}

func applySuffix(eff *Effect, tok string) error {
	switch {
	case tok == "once":
		eff.Once = true
		return nil
	case strings.HasPrefix(tok, "repo="):
		eff.MatchRepo = strings.TrimPrefix(tok, "repo=")
		return nil
	case strings.HasPrefix(tok, "issue="):
		v := strings.TrimPrefix(tok, "issue=")
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("issue= needs integer, got %q", v)
		}
		eff.MatchIssue = n
		return nil
	default:
		return fmt.Errorf("unknown suffix")
	}
}

// splitArgs splits "a,b,c" trimming whitespace; empty input yields nil.
// It does not currently support escaped commas inside argument values — the
// grammar is intentionally simple. Callers needing literal commas in error
// messages should use the Arm() API instead of the env var.
func splitArgs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
