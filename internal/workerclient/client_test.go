package workerclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAddsBearerTokenToTaskEndpoints(t *testing.T) {
	var headers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := New(srv.URL, "kid.secret", srv.Client())

	check := func(resp *http.Response, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}

	check(client.PollTask(context.Background(), "worker-1", 0))
	check(client.Heartbeat(context.Background(), "task-1"))
	check(client.SubmitResult(context.Background(), "task-1", map[string]string{"status": "ok"}))

	if len(headers) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(headers))
	}
	for i, header := range headers {
		if header != "Bearer kid.secret" {
			t.Fatalf("header[%d] = %q", i, header)
		}
	}
}
