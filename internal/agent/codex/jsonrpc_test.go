package codex

import (
	"encoding/json"
	"testing"
)

func TestRequestMarshal(t *testing.T) {
	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params:  map[string]string{"key": "value"},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal Request: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	// Check jsonrpc field.
	var jsonrpc string
	if err := json.Unmarshal(raw["jsonrpc"], &jsonrpc); err != nil {
		t.Fatalf("unmarshal jsonrpc: %v", err)
	}
	if jsonrpc != "2.0" {
		t.Fatalf("jsonrpc = %q, want %q", jsonrpc, "2.0")
	}

	// Check id field.
	var id int64
	if err := json.Unmarshal(raw["id"], &id); err != nil {
		t.Fatalf("unmarshal id: %v", err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}

	// Check method field.
	var method string
	if err := json.Unmarshal(raw["method"], &method); err != nil {
		t.Fatalf("unmarshal method: %v", err)
	}
	if method != "initialize" {
		t.Fatalf("method = %q, want %q", method, "initialize")
	}

	// Check params field exists.
	if _, ok := raw["params"]; !ok {
		t.Fatal("params field missing from JSON")
	}
}

func TestRequestMarshalNoParams(t *testing.T) {
	req := Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "shutdown",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal Request: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	// params should be omitted when nil.
	if _, ok := raw["params"]; ok {
		t.Fatal("params should be omitted when nil")
	}
}

func TestResponseUnmarshal(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`

	var resp Response
	if err := json.Unmarshal([]byte(input), &resp); err != nil {
		t.Fatalf("unmarshal Response: %v", err)
	}

	if resp.JSONRPC != "2.0" {
		t.Fatalf("JSONRPC = %q, want %q", resp.JSONRPC, "2.0")
	}
	if resp.ID != 1 {
		t.Fatalf("ID = %d, want 1", resp.ID)
	}
	if resp.Error != nil {
		t.Fatalf("Error = %v, want nil", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("Result is nil, want non-nil")
	}

	// Verify result content.
	var result map[string]string
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result["status"] != "ok" {
		t.Fatalf("result[status] = %q, want %q", result["status"], "ok")
	}
}

func TestResponseUnmarshalError(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":2,"error":{"code":-32600,"message":"Invalid Request"}}`

	var resp Response
	if err := json.Unmarshal([]byte(input), &resp); err != nil {
		t.Fatalf("unmarshal Response: %v", err)
	}

	if resp.ID != 2 {
		t.Fatalf("ID = %d, want 2", resp.ID)
	}
	if resp.Error == nil {
		t.Fatal("Error is nil, want non-nil")
	}
	if resp.Error.Code != -32600 {
		t.Fatalf("Error.Code = %d, want %d", resp.Error.Code, -32600)
	}
	if resp.Error.Message != "Invalid Request" {
		t.Fatalf("Error.Message = %q, want %q", resp.Error.Message, "Invalid Request")
	}

	// RPCError should implement error interface.
	errMsg := resp.Error.Error()
	if errMsg != "Invalid Request" {
		t.Fatalf("Error() = %q, want %q", errMsg, "Invalid Request")
	}
}

func TestNotificationUnmarshal(t *testing.T) {
	input := `{"jsonrpc":"2.0","method":"codex/event","params":{"kind":"message","data":"hello"}}`

	var notif Notification
	if err := json.Unmarshal([]byte(input), &notif); err != nil {
		t.Fatalf("unmarshal Notification: %v", err)
	}

	if notif.JSONRPC != "2.0" {
		t.Fatalf("JSONRPC = %q, want %q", notif.JSONRPC, "2.0")
	}
	if notif.Method != "codex/event" {
		t.Fatalf("Method = %q, want %q", notif.Method, "codex/event")
	}
	if notif.Params == nil {
		t.Fatal("Params is nil, want non-nil")
	}

	var params map[string]string
	if err := json.Unmarshal(notif.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params["kind"] != "message" {
		t.Fatalf("params[kind] = %q, want %q", params["kind"], "message")
	}
}

func TestResponseRoundTrip(t *testing.T) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      42,
		Result:  json.RawMessage(`{"value":true}`),
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.ID != 42 {
		t.Fatalf("ID = %d, want 42", got.ID)
	}
	if string(got.Result) != `{"value":true}` {
		t.Fatalf("Result = %s, want %s", got.Result, `{"value":true}`)
	}
}
