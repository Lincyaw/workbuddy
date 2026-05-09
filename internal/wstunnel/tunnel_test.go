package wstunnel

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func tunnelPair(t *testing.T, handler http.Handler) (*Endpoint, func()) {
	t.Helper()
	serverEPCh := make(chan *Endpoint, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		ep := NewEndpoint(c)
		serverEPCh <- ep
		go ep.ServeRequests(r.Context(), handler)
		_ = ep.Run(r.Context())
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	clientEP := NewEndpoint(c)
	go func() { _ = clientEP.Run(context.Background()) }()
	select {
	case <-serverEPCh:
	case <-ctx.Done():
		t.Fatalf("server endpoint not ready: %v", ctx.Err())
	}
	return clientEP, func() { _ = clientEP.Close(); cancel(); srv.Close() }
}

func TestRoundTrip(t *testing.T) {
	ep, cleanup := tunnelPair(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/echo?x=1" {
			t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(append([]byte("ok:"), body...))
	}))
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, "http://worker/echo?x=1", bytes.NewBufferString("hello"))
	resp, err := ep.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated || string(body) != "ok:hello" || resp.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("resp status=%d ct=%q body=%q", resp.StatusCode, resp.Header.Get("Content-Type"), string(body))
	}
}

func TestConcurrentStreams(t *testing.T) {
	ep, cleanup := tunnelPair(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Query().Get("i")))
	}))
	defer cleanup()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, "http://worker/sleep?i="+strconv.Itoa(i), nil)
			resp, err := ep.Do(context.Background(), req)
			if err != nil {
				errs <- err.Error()
				return
			}
			body, _ := io.ReadAll(resp.Body)
			if string(body) != strconv.Itoa(i) {
				errs <- "bad body " + string(body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPeerDisconnectUnblocksStream(t *testing.T) {
	block := make(chan struct{})
	ep, cleanup := tunnelPair(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		_, _ = w.Write([]byte("late"))
	}))
	defer close(block)
	defer cleanup()

	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, "http://worker/block", nil)
		_, err := ep.Do(context.Background(), req)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = ep.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error")
		}
	case <-time.After(time.Second):
		t.Fatal("Do did not unblock after disconnect")
	}
}

func TestFrameParseErrorClosesTunnel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ep := NewEndpoint(c)
		_ = ep.Run(r.Context())
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("not-json\nbody")); err != nil {
		t.Fatalf("write bad frame: %v", err)
	}
	_, _, err = c.Read(ctx)
	if err == nil {
		t.Fatal("expected connection close after parse error")
	}
}

func TestRegistryRemoveOnlyCurrentTunnel(t *testing.T) {
	r := NewRegistry()
	ep1 := NewEndpoint(nil)
	ep2 := NewEndpoint(nil)
	r.Register("worker-1", ep1)
	r.Register("worker-1", ep2)
	if removed := r.Remove("worker-1", ep1); removed {
		t.Fatal("old endpoint removal should not evict replacement tunnel")
	}
	if st := r.Status("worker-1"); !st.Connected {
		t.Fatal("replacement tunnel should remain connected")
	}
	if removed := r.Remove("worker-1", ep2); !removed {
		t.Fatal("current endpoint removal should succeed")
	}
	if st := r.Status("worker-1"); st.Connected {
		t.Fatal("current endpoint removal should clear tunnel")
	}
}
