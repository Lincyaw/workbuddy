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
	if _, ok := raw["params"]; !ok {
		t.Fatal("params field missing from JSON")
	}
}

func TestResponseUnmarshalStringOrNumericID(t *testing.T) {
	cases := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`,
		`{"jsonrpc":"2.0","id":"req-1","result":{"status":"ok"}}`,
	}
	for _, input := range cases {
		var resp Response
		if err := json.Unmarshal([]byte(input), &resp); err != nil {
			t.Fatalf("unmarshal Response: %v", err)
		}
		if requestIDKey(resp.ID) == "" {
			t.Fatalf("requestIDKey(%s) is empty", resp.ID)
		}
		if resp.Error != nil {
			t.Fatalf("Error = %v, want nil", resp.Error)
		}
	}
}

func TestServerRequestUnmarshal(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":"approval-1","method":"item/commandExecution/requestApproval","params":{"threadId":"t-1"}}`

	var req ServerRequest
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		t.Fatalf("unmarshal ServerRequest: %v", err)
	}
	if req.Method != "item/commandExecution/requestApproval" {
		t.Fatalf("Method = %q", req.Method)
	}
	if requestIDKey(req.ID) != `"approval-1"` {
		t.Fatalf("ID key = %q", requestIDKey(req.ID))
	}
}

func TestRPCErrorError(t *testing.T) {
	err := (&RPCError{Code: -32600, Message: "Invalid Request"}).Error()
	if err != "Invalid Request" {
		t.Fatalf("Error() = %q, want %q", err, "Invalid Request")
	}
}
