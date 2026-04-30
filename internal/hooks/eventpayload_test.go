package hooks

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildEnvelopeBasic(t *testing.T) {
	ev := Event{
		Type:      "state_entry",
		Repo:      "Lincyaw/workbuddy",
		IssueNum:  250,
		Payload:   []byte(`{"state":"developing"}`),
		Timestamp: time.Date(2026, 4, 30, 3, 23, 27, 0, time.UTC),
	}
	env := BuildEnvelope(ev)
	if env.SchemaVersion != 1 {
		t.Fatalf("schema version: %d", env.SchemaVersion)
	}
	if env.EventType != "state_entry" || env.Repo != "Lincyaw/workbuddy" || env.IssueNum != 250 {
		t.Fatalf("envelope mismatch: %+v", env)
	}
	if env.Timestamp != "2026-04-30T03:23:27Z" {
		t.Fatalf("timestamp: %q", env.Timestamp)
	}
	out, err := MarshalEnvelope(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data := back["data"].(map[string]any)
	if data["state"] != "developing" {
		t.Fatalf("inner data not preserved: %v", back)
	}
}

func TestBuildEnvelopeNilPayload(t *testing.T) {
	env := BuildEnvelope(Event{Type: "poll", Repo: "x/y"})
	if string(env.Data) != "null" {
		t.Fatalf("nil payload should serialize to null, got %s", env.Data)
	}
}

func TestBuildEnvelopeForEachEventType(t *testing.T) {
	// One representative case per category to lock the translation in.
	cases := []struct {
		name     string
		evType   string
		payload  string
		wantData string
	}{
		{"transition", "transition", `{"from":"a","to":"b"}`, `{"from":"a","to":"b"}`},
		{"alert", "alert", `{"severity":"warn","message":"m"}`, `{"severity":"warn","message":"m"}`},
		{"completed", "completed", `{"agent":"dev"}`, `{"agent":"dev"}`},
		{"empty", "poll", ``, `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := BuildEnvelope(Event{Type: c.evType, Payload: []byte(c.payload)})
			if string(env.Data) != c.wantData {
				t.Fatalf("data: got %s want %s", env.Data, c.wantData)
			}
			if env.EventType != c.evType {
				t.Fatalf("event type lost: %q", env.EventType)
			}
		})
	}
}

func TestIsHookSelfEvent(t *testing.T) {
	if !IsHookSelfEvent("hook_overflow") || !IsHookSelfEvent("hook_failed") {
		t.Fatalf("hook_ prefix should be self event")
	}
	if IsHookSelfEvent("dispatch") || IsHookSelfEvent("alert") {
		t.Fatalf("normal events misclassified as self events")
	}
}
