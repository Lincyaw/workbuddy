package hooks

import (
	"encoding/json"
	"strings"
	"time"
)

// PayloadSchemaVersion identifies the v1 envelope. Field set is append-only;
// removals or renames must bump this constant.
const PayloadSchemaVersion = 1

// HookEventPrefix is the eventlog type prefix reserved for hook self-events
// (hook_overflow, hook_failed, hook_disabled, ...). Events with this prefix
// are written to the SQLite event log but skipped by the dispatcher to
// prevent self-amplification loops.
const HookEventPrefix = "hook_"

// Event captures the slice of an eventlog entry the dispatcher needs to know
// about. Wider eventlog fields (id, ts in DB, etc.) are not relevant to user
// actions and are deliberately omitted from the v1 envelope.
type Event struct {
	Type     string
	Repo     string
	IssueNum int
	// Payload is the JSON-marshalled inner payload of the eventlog entry. May
	// be empty/nil when the eventlog entry has no payload.
	Payload []byte
	// Timestamp is the time the event was logged. Defaults to time.Now() when
	// zero.
	Timestamp time.Time
}

// IsHookSelfEvent returns true if the event was emitted by the hook system
// itself and must not re-enter the dispatcher.
func IsHookSelfEvent(eventType string) bool {
	return strings.HasPrefix(eventType, HookEventPrefix)
}

// Envelope is the stable v1 payload delivered to actions. Fields are
// append-only; do not rename or remove without bumping PayloadSchemaVersion.
type Envelope struct {
	SchemaVersion int             `json:"schema_version"`
	EventType     string          `json:"event_type"`
	Timestamp     string          `json:"ts"`
	Repo          string          `json:"repo"`
	IssueNum      int             `json:"issue_num"`
	Data          json.RawMessage `json:"data"`
}

// BuildEnvelope translates an internal eventlog event into the stable v1
// payload sent to user actions.
func BuildEnvelope(ev Event) Envelope {
	ts := ev.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	data := json.RawMessage(ev.Payload)
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	return Envelope{
		SchemaVersion: PayloadSchemaVersion,
		EventType:     ev.Type,
		Timestamp:     ts.UTC().Format(time.RFC3339Nano),
		Repo:          ev.Repo,
		IssueNum:      ev.IssueNum,
		Data:          data,
	}
}

// MarshalEnvelope is a small helper around json.Marshal that returns "null"
// data on errors so the dispatcher never crashes on a malformed inner
// payload — a degraded delivery is preferable to losing the event entirely.
func MarshalEnvelope(env Envelope) ([]byte, error) {
	return json.Marshal(env)
}
