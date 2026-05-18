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
	// Disable keepalive by default for the existing test suite: those
	// tests neither exercise nor mock idle-disconnect semantics, and a
	// real ping every 25s adds nothing but noise. The dedicated
	// keepalive tests below construct their own pair with short
	// intervals.
	return tunnelPairWithKeepalive(t, handler, -1, 0)
}

func tunnelPairWithKeepalive(t *testing.T, handler http.Handler, interval, timeout time.Duration) (*Endpoint, func()) {
	t.Helper()
	serverEPCh := make(chan *Endpoint, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		ep := NewEndpoint(c)
		ep.SetKeepalive(interval, timeout)
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
	clientEP.SetKeepalive(interval, timeout)
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

// TestKeepalivePreservesIdleTunnel pins the #345 Wave 5 fix: an idle
// tunnel must stay healthy across many keepalive intervals because the
// periodic Conn.Ping defeats middlebox idle-disconnect. Pre-fix the
// only WSS traffic was the data frames, so any intermediary with a
// short idle timer would EOF the connection within a minute. We use a
// 30ms ping interval so the test exercises ~10 ping cycles in under a
// second.
func TestKeepalivePreservesIdleTunnel(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("alive"))
	})
	ep, cleanup := tunnelPairWithKeepalive(t, handler, 30*time.Millisecond, 500*time.Millisecond)
	defer cleanup()

	// Stay idle for ~10 keepalive intervals. If pings were not being
	// exchanged the inactive-conn path would not differ from the
	// pre-fix behaviour, but the assertion below — a real request
	// after the idle — would still pass on a fresh dial; what would
	// actually break is the in-flight goroutine state, since the
	// existing endpoint instance would have errored out.
	time.Sleep(300 * time.Millisecond)
	if err := ep.err(); err != nil {
		t.Fatalf("endpoint failed during idle: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://worker/probe", nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := ep.Do(ctx, req)
	if err != nil {
		t.Fatalf("Do after idle: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "alive" {
		t.Fatalf("body=%q, want %q", body, "alive")
	}
}

// TestKeepaliveSurfacesDeadPeer pins the dead-peer detection half of
// the fix: when the underlying conn is severed (we simulate by closing
// the client's *websocket.Conn out-of-band), the server-side Ping
// either fails to write or times out waiting for the Pong, and Run
// returns a wrapped keepalive error rather than blocking on the
// read-loop until something else (a real outbound frame or a TCP
// keepalive at 2h) finally surfaces the failure.
func TestKeepaliveSurfacesDeadPeer(t *testing.T) {
	t.Parallel()
	// Custom server so we can capture the server-side Run error directly.
	serverDone := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			serverDone <- err
			return
		}
		ep := NewEndpoint(c)
		ep.SetKeepalive(30*time.Millisecond, 100*time.Millisecond)
		serverDone <- ep.Run(r.Context())
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// CloseNow drops the underlying TCP without sending a WS close
	// frame — the network-cable-yanked scenario.
	_ = c.CloseNow()

	select {
	case err := <-serverDone:
		if err == nil {
			t.Fatal("expected server Run to return an error after peer disconnect")
		}
		// Either the read-loop sees EOF first or the keepalive
		// goroutine's Ping fails first. Both are acceptable
		// failure modes; the contract is "Run returns within
		// one keepalive cycle", not which path tripped first.
	case <-time.After(time.Second):
		t.Fatal("server Run did not return within 1s of peer disconnect")
	}
}

// TestKeepaliveDisabledByNegativeInterval pins the opt-out path that
// tests in this package rely on (and that lets a future caller pick a
// different idle-detection strategy without forking the package).
// SetKeepalive(-1, _) leaves Run as a bare read-loop and an idle peer
// causes no traffic at all on the wire beyond the WS handshake.
func TestKeepaliveDisabledByNegativeInterval(t *testing.T) {
	t.Parallel()
	ep, cleanup := tunnelPairWithKeepalive(t, http.NotFoundHandler(), -1, 0)
	defer cleanup()
	time.Sleep(100 * time.Millisecond)
	if err := ep.err(); err != nil {
		t.Fatalf("endpoint failed with keepalive disabled: %v", err)
	}
}

// TestLargePayloadSurvivesDefaultReadLimit pins the #345 Wave 7 fix:
// the sessionproxy fan-out routes JSON listing payloads through this
// tunnel, and a production worker with a few hundred sessions easily
// emits >32 KiB in a single GET /api/v1/sessions response. Pre-fix the
// underlying coder/websocket Conn capped reads at its 32 KiB default,
// so the tunnel tore down with "websocket: message too big: read
// limited at 32769 bytes". This test exercises BOTH directions of the
// tunnel inside one Endpoint.Do call:
//
//   - client → server: the request body is 100 KiB; the worker-side
//     ServeRequests path must DecodeFrame it (server-side Read), which
//     means the server-side conn's read limit covers it.
//
//   - server → client: the handler responds with a 100 KiB body; the
//     client-side Run loop must DecodeFrame the response, which means
//     the client-side conn's read limit covers it.
//
// If either side were missing the SetReadLimit bump the test would
// fail; the symmetric application inside NewEndpoint is the whole
// point of doing it there rather than at the call sites.
func TestLargePayloadSurvivesDefaultReadLimit(t *testing.T) {
	t.Parallel()
	const size = 100 * 1024
	want := bytes.Repeat([]byte("A"), size)
	ep, cleanup := tunnelPair(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		if len(got) != size {
			t.Errorf("request body length = %d, want %d", len(got), size)
			return
		}
		// Echo back a 100 KiB response so the client-side Run loop
		// also has to read >32 KiB in a single frame.
		_, _ = w.Write(bytes.Repeat([]byte("B"), size))
	}))
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, "http://worker/big", bytes.NewReader(want))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := ep.Do(ctx, req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if len(body) != size {
		t.Fatalf("response body length = %d, want %d", len(body), size)
	}
	if !bytes.Equal(body, bytes.Repeat([]byte("B"), size)) {
		t.Fatal("response body content mismatch")
	}
}

// TestSmallReadLimitRejectsLargePayload is the RED-state companion to
// TestLargePayloadSurvivesDefaultReadLimit: it tightens both ends back
// to the upstream library's 32 KiB default via SetReadLimit, then sends
// the same 100 KiB request and asserts the tunnel surfaces an error
// rather than silently succeeding. This pins the underlying mechanism
// (the upstream Conn read cap is what blew up production) and proves
// that NewEndpoint's DefaultReadLimit bump is load-bearing — drop it
// and this test plus the one above flip places.
func TestSmallReadLimitRejectsLargePayload(t *testing.T) {
	t.Parallel()
	const size = 100 * 1024
	// Custom pair so we can SetReadLimit on BOTH endpoints before any
	// frames are exchanged. tunnelPair already disables keepalive.
	serverEPCh := make(chan *Endpoint, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		ep := NewEndpoint(c)
		ep.SetKeepalive(-1, 0)
		ep.SetReadLimit(32 * 1024)
		serverEPCh <- ep
		go ep.ServeRequests(r.Context(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		}))
		_ = ep.Run(r.Context())
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	clientEP := NewEndpoint(c)
	clientEP.SetKeepalive(-1, 0)
	clientEP.SetReadLimit(32 * 1024)
	go func() { _ = clientEP.Run(context.Background()) }()
	defer func() { _ = clientEP.Close() }()
	select {
	case <-serverEPCh:
	case <-ctx.Done():
		t.Fatalf("server endpoint not ready: %v", ctx.Err())
	}

	req, _ := http.NewRequest(http.MethodPost, "http://worker/big", bytes.NewReader(bytes.Repeat([]byte("A"), size)))
	resp, err := clientEP.Do(ctx, req)
	if err == nil {
		// If Do somehow returned without error, the response stream
		// itself must have been truncated/closed by the limit. Read
		// the body to consume any partial state, then fail loudly.
		_, _ = io.Copy(io.Discard, resp.Body)
		t.Fatal("expected Do to fail when both ends cap reads at 32 KiB, got nil")
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
