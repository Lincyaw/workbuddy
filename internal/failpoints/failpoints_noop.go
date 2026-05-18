//go:build !faultinject

package failpoints

// This file is the default-build implementation. Every entry point is a
// trivial no-op so the compiler can inline and fold the call away.

// Trip always returns nil.
func Trip(name string, opts ...MatchOpt) *Effect {
	_ = name
	_ = opts
	return nil
}

// Hit always returns nil.
func Hit(name string, opts ...MatchOpt) error {
	_ = name
	_ = opts
	return nil
}

// Arm is a no-op in the default build and always returns false.
func Arm(name string, eff Effect) bool {
	_ = name
	_ = eff
	return false
}

// Disarm is a no-op in the default build.
func Disarm(name string) { _ = name }

// Reset is a no-op in the default build.
func Reset() {}

// Enabled reports false in the default build.
func Enabled() bool { return false }
