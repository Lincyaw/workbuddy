package hooks

import (
	"context"
	"fmt"
	"sync"
)

// Action types known to Phase 1a.
const (
	ActionTypeWebhook = "webhook"
	ActionTypeCommand = "command"
)

// Action is the runtime contract every action implementation satisfies. The
// payload is a stable v1 envelope (see eventpayload.go); already JSON-encoded.
type Action interface {
	// Type returns the action type identifier (e.g. "webhook").
	Type() string
	// Execute runs the action. ctx cancellation must be respected.
	Execute(ctx context.Context, payload []byte) error
}

// ActionRegistry registers concrete Action implementations bound to a Hook.
// New action types only require a registration call — the dispatcher itself
// never changes.
type ActionRegistry struct {
	mu       sync.RWMutex
	builders map[string]ActionBuilder
}

// ActionBuilder constructs an Action from a parsed hook definition. It may
// return startup warnings (e.g. unresolved env var in headers) alongside the
// instance.
type ActionBuilder func(h *Hook) (Action, []string, error)

// NewActionRegistry returns an empty registry. Most callers want
// DefaultActionRegistry which has the built-in actions pre-registered.
func NewActionRegistry() *ActionRegistry {
	return &ActionRegistry{builders: map[string]ActionBuilder{}}
}

// Register installs a builder for the given action type.
func (r *ActionRegistry) Register(actionType string, b ActionBuilder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.builders[actionType] = b
}

// Build constructs the Action attached to a hook.
func (r *ActionRegistry) Build(h *Hook) (Action, []string, error) {
	r.mu.RLock()
	b, ok := r.builders[h.Action.Type]
	r.mu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("hooks: no builder registered for action type %q", h.Action.Type)
	}
	return b(h)
}

// DefaultActionRegistry returns a registry seeded with the actions shipped in
// the current phase. Phase 1a: webhook only.
func DefaultActionRegistry() *ActionRegistry {
	r := NewActionRegistry()
	r.Register(ActionTypeWebhook, buildWebhookAction)
	return r
}
