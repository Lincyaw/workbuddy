package wstunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lincyaw/workbuddy/internal/failpoints"
	"github.com/coder/websocket"
)

const (
	FrameRequest  = "request"
	FrameResponse = "response"
	FrameClose    = "close"
)

var ErrClosed = errors.New("wstunnel: closed")

// DefaultKeepaliveInterval is how often Endpoint.Run sends a WSS ping to the
// peer when keepalive is enabled and the caller has not overridden the
// interval. 25s is short enough to defeat the common 30-60s middlebox idle
// timeouts (cheap VPS NATs, cloud load balancers, conntrack on long-lived
// WAN paths) and long enough that the per-tick CPU cost is negligible.
//
// Picked over the library's TCP-keepalive suggestion because the default
// Linux TCP keepalive (`tcp_keepalive_time=7200s`) is two hours: far longer
// than any reasonable middlebox idle window. The #345 Wave 5 postmortem
// observed coordinator↔worker tunnel disconnect/reconnect at ~minute
// cadence with "failed to read frame header: EOF" — the exact signature
// of a peer/middlebox closing an idle WSS connection without a close frame.
const DefaultKeepaliveInterval = 25 * time.Second

// DefaultKeepaliveTimeout is the per-ping deadline: how long Run will wait
// for the matching Pong before declaring the connection dead and tearing
// it down so the caller's reconnect loop runs. 20s gives a generous margin
// over realistic RTT (single-digit-ms LAN, ~100ms transcontinental WAN) so
// transient jitter does not flap healthy tunnels, while still surfacing a
// truly dead peer within one keepalive cycle.
const DefaultKeepaliveTimeout = 20 * time.Second

type Frame struct {
	Type     string      `json:"type"`
	StreamID string      `json:"stream_id"`
	Method   string      `json:"method,omitempty"`
	Path     string      `json:"path,omitempty"`
	Headers  http.Header `json:"headers,omitempty"`
	Status   int         `json:"status,omitempty"`
	End      bool        `json:"end,omitempty"`
	Error    string      `json:"error,omitempty"`
	Body     []byte      `json:"-"`
}

type Endpoint struct {
	conn     *websocket.Conn
	writeMu  sync.Mutex
	mu       sync.Mutex
	streams  map[string]chan Frame
	requests chan Frame
	closed   chan struct{}
	closeErr error
	next     atomic.Uint64

	// keepaliveInterval and keepaliveTimeout control the periodic WSS
	// ping that Run sends to keep middleboxes from idle-closing the
	// connection. Zero values mean "use the package defaults"; a
	// negative value disables the ping goroutine entirely (used by a
	// handful of tests that need to observe the bare read-loop
	// behaviour). Mutated only by SetKeepalive before Run starts.
	keepaliveInterval time.Duration
	keepaliveTimeout  time.Duration
}

func NewEndpoint(conn *websocket.Conn) *Endpoint {
	return &Endpoint{
		conn:     conn,
		streams:  make(map[string]chan Frame),
		requests: make(chan Frame, 64),
		closed:   make(chan struct{}),
	}
}

// SetKeepalive overrides the ping interval and per-ping timeout used by
// Run's keepalive goroutine. interval <= 0 disables keepalive (Run reverts
// to a pure read-loop, matching pre-#345-Wave-5 behaviour). interval > 0
// with timeout <= 0 falls back to DefaultKeepaliveTimeout. Must be called
// before Run; safe to call once at endpoint construction.
func (e *Endpoint) SetKeepalive(interval, timeout time.Duration) {
	e.keepaliveInterval = interval
	e.keepaliveTimeout = timeout
}

func (e *Endpoint) Run(ctx context.Context) error {
	if stop := e.startKeepalive(ctx); stop != nil {
		defer stop()
	}
	for {
		_, data, err := e.conn.Read(ctx)
		if err != nil {
			e.fail(err)
			return err
		}
		frame, err := DecodeFrame(data)
		if err != nil {
			e.fail(err)
			_ = e.conn.Close(websocket.StatusUnsupportedData, "invalid tunnel frame")
			return err
		}
		switch frame.Type {
		case FrameResponse, FrameClose:
			e.mu.Lock()
			ch := e.streams[frame.StreamID]
			if frame.Type == FrameClose || frame.End {
				delete(e.streams, frame.StreamID)
			}
			e.mu.Unlock()
			if ch != nil {
				select {
				case ch <- frame:
				case <-ctx.Done():
				}
				if frame.Type == FrameClose || frame.End {
					close(ch)
				}
			}
		case FrameRequest:
			select {
			case e.requests <- frame:
			case <-ctx.Done():
				e.fail(ctx.Err())
				return ctx.Err()
			}
		default:
			err := fmt.Errorf("wstunnel: unknown frame type %q", frame.Type)
			e.fail(err)
			_ = e.conn.Close(websocket.StatusUnsupportedData, "unknown tunnel frame type")
			return err
		}
	}
}

func (e *Endpoint) Requests() <-chan Frame { return e.requests }

func (e *Endpoint) Close() error {
	e.fail(ErrClosed)
	if e.conn == nil {
		return nil
	}
	return e.conn.Close(websocket.StatusNormalClosure, "closed")
}

func (e *Endpoint) fail(err error) {
	if err == nil {
		err = ErrClosed
	}
	e.mu.Lock()
	select {
	case <-e.closed:
		e.mu.Unlock()
		return
	default:
	}
	e.closeErr = err
	for id, ch := range e.streams {
		delete(e.streams, id)
		close(ch)
	}
	close(e.requests)
	close(e.closed)
	e.mu.Unlock()
}

// startKeepalive launches the periodic WSS ping goroutine and returns a
// stop function that the Run caller defers. Returns nil (and the caller
// defers nothing) when keepalive is disabled or the endpoint has no
// connection (test endpoints used for registry semantics use NewEndpoint
// with a nil conn).
//
// On ping failure (timeout, write error, or peer close) the goroutine
// calls e.fail(err) and closes the underlying conn so the outer Read in
// Run unblocks promptly and the caller's reconnect loop runs. This makes
// dead-peer detection bounded by keepaliveInterval+keepaliveTimeout
// rather than waiting for the next outbound frame attempt or a TCP RST.
func (e *Endpoint) startKeepalive(ctx context.Context) func() {
	if e.conn == nil {
		return nil
	}
	interval := e.keepaliveInterval
	if interval == 0 {
		interval = DefaultKeepaliveInterval
	}
	if interval < 0 {
		return nil
	}
	timeout := e.keepaliveTimeout
	if timeout <= 0 {
		timeout = DefaultKeepaliveTimeout
	}
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-e.closed:
				return
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(ctx, timeout)
				err := e.conn.Ping(pingCtx)
				cancel()
				if err != nil {
					// Mark the endpoint failed first so the
					// Run reader sees closeErr instead of a
					// bare io.EOF, then close the conn so
					// the blocking Read returns immediately.
					e.fail(fmt.Errorf("wstunnel: keepalive ping failed: %w", err))
					_ = e.conn.Close(websocket.StatusPolicyViolation, "keepalive ping failed")
					return
				}
			}
		}
	}()
	return func() {
		close(stopCh)
		<-done
	}
}

func (e *Endpoint) send(ctx context.Context, frame Frame) error {
	select {
	case <-e.closed:
		return e.closeErr
	default:
	}
	// Failpoint: simulate a dropped outbound tunnel frame. Endpoint.send
	// carries every frame type (FrameRequest, FrameClose, FrameResponse,
	// and any heartbeat frames layered on top), so the hook is named for
	// the generic send path rather than a specific frame type. The "drop"
	// effect kind silently no-ops the write and returns nil so the caller
	// sees a successful send while the wire never sees the bytes — exactly
	// the failure mode the #345 silent-stall postmortem flagged.
	if err := failpoints.Hit("wstunnel.send.drop"); err != nil {
		if errors.Is(err, failpoints.ErrFailpointDrop) {
			return nil
		}
		return err
	}
	data, err := EncodeFrame(frame)
	if err != nil {
		return err
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	return e.conn.Write(ctx, websocket.MessageBinary, data)
}

func (e *Endpoint) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("wstunnel: request URL is required")
	}
	body, err := readBody(req.Body)
	if err != nil {
		return nil, err
	}
	streamID := strconv.FormatUint(e.next.Add(1), 10)
	ch := make(chan Frame, 16)
	e.mu.Lock()
	select {
	case <-e.closed:
		err := e.closeErr
		e.mu.Unlock()
		return nil, err
	default:
	}
	e.streams[streamID] = ch
	e.mu.Unlock()

	path := req.URL.RequestURI()
	if path == "" {
		path = "/"
	}
	if err := e.send(ctx, Frame{Type: FrameRequest, StreamID: streamID, Method: req.Method, Path: path, Headers: cloneHeader(req.Header), Body: body, End: true}); err != nil {
		e.removeStream(streamID)
		return nil, err
	}

	var status int
	headers := http.Header{}
	var buf bytes.Buffer
	for {
		select {
		case frame, ok := <-ch:
			if !ok {
				if e.err() != nil {
					return nil, e.err()
				}
				return nil, io.EOF
			}
			if frame.Type == FrameClose {
				if frame.Error != "" {
					return nil, errors.New(frame.Error)
				}
				return nil, io.EOF
			}
			if frame.Status != 0 {
				status = frame.Status
				headers = cloneHeader(frame.Headers)
			}
			_, _ = buf.Write(frame.Body)
			if frame.End {
				if status == 0 {
					status = http.StatusBadGateway
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(bytes.NewReader(buf.Bytes()))}, nil
			}
		case <-ctx.Done():
			e.removeStream(streamID)
			_ = e.send(context.Background(), Frame{Type: FrameClose, StreamID: streamID, Error: ctx.Err().Error(), End: true})
			return nil, ctx.Err()
		}
	}
}

func (e *Endpoint) ServeRequests(ctx context.Context, handler http.Handler) {
	if handler == nil {
		handler = http.NotFoundHandler()
	}
	for frame := range e.Requests() {
		f := frame
		go e.serveOne(ctx, handler, f)
	}
}

func (e *Endpoint) serveOne(ctx context.Context, handler http.Handler, frame Frame) {
	method := strings.TrimSpace(frame.Method)
	if method == "" {
		method = http.MethodGet
	}
	target := frame.Path
	if target == "" || !strings.HasPrefix(target, "/") {
		target = "/"
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(frame.Body))
	req = req.WithContext(ctx)
	req.Header = cloneHeader(frame.Headers)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	_ = e.send(ctx, Frame{Type: FrameResponse, StreamID: frame.StreamID, Status: resp.StatusCode, Headers: cloneHeader(resp.Header), Body: body, End: true})
}

func (e *Endpoint) removeStream(id string) {
	e.mu.Lock()
	if ch := e.streams[id]; ch != nil {
		delete(e.streams, id)
		close(ch)
	}
	e.mu.Unlock()
}

func (e *Endpoint) err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closeErr
}

func EncodeFrame(f Frame) ([]byte, error) {
	head := f
	head.Body = nil
	b, err := json.Marshal(head)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(b)+1+len(f.Body))
	out = append(out, b...)
	out = append(out, '\n')
	out = append(out, f.Body...)
	return out, nil
}

func DecodeFrame(data []byte) (Frame, error) {
	head, body, ok := bytes.Cut(data, []byte("\n"))
	if !ok {
		return Frame{}, fmt.Errorf("wstunnel: missing frame separator")
	}
	var f Frame
	if err := json.Unmarshal(head, &f); err != nil {
		return Frame{}, fmt.Errorf("wstunnel: decode frame header: %w", err)
	}
	if strings.TrimSpace(f.Type) == "" || strings.TrimSpace(f.StreamID) == "" {
		return Frame{}, fmt.Errorf("wstunnel: type and stream_id are required")
	}
	f.Body = append([]byte(nil), body...)
	if f.Headers != nil {
		f.Headers = canonicalHeader(f.Headers)
	}
	return f, nil
}

func readBody(rc io.ReadCloser) ([]byte, error) {
	if rc == nil || rc == http.NoBody {
		return nil, nil
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vals := range h {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		out[ck] = append([]string(nil), vals...)
	}
	return out
}

func canonicalHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vals := range h {
		out[textproto.CanonicalMIMEHeaderKey(k)] = vals
	}
	return out
}

type Registry struct {
	mu      sync.RWMutex
	tunnels map[string]*Tunnel
	now     func() time.Time
}

type Tunnel struct {
	WorkerID      string
	Endpoint      *Endpoint
	LastHandshake time.Time
}

type Status struct {
	Connected     bool      `json:"connected"`
	LastHandshake time.Time `json:"last_handshake,omitempty"`
}

func NewRegistry() *Registry {
	return &Registry{tunnels: map[string]*Tunnel{}, now: time.Now}
}

func (r *Registry) Register(workerID string, ep *Endpoint) *Tunnel {
	if r == nil || ep == nil || strings.TrimSpace(workerID) == "" {
		return nil
	}
	t := &Tunnel{WorkerID: strings.TrimSpace(workerID), Endpoint: ep, LastHandshake: r.now()}
	r.mu.Lock()
	old := r.tunnels[t.WorkerID]
	r.tunnels[t.WorkerID] = t
	r.mu.Unlock()
	if old != nil && old.Endpoint != nil {
		_ = old.Endpoint.Close()
	}
	return t
}

func (r *Registry) Remove(workerID string, ep *Endpoint) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.tunnels[workerID]; cur != nil && (ep == nil || cur.Endpoint == ep) {
		delete(r.tunnels, workerID)
		return true
	}
	return false
}

func (r *Registry) Do(ctx context.Context, workerID string, req *http.Request) (*http.Response, error) {
	r.mu.RLock()
	t := r.tunnels[strings.TrimSpace(workerID)]
	r.mu.RUnlock()
	if t == nil || t.Endpoint == nil {
		return nil, ErrClosed
	}
	return t.Endpoint.Do(ctx, req)
}

func (r *Registry) Status(workerID string) Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if t := r.tunnels[strings.TrimSpace(workerID)]; t != nil {
		return Status{Connected: true, LastHandshake: t.LastHandshake}
	}
	return Status{}
}

func (r *Registry) Snapshot() map[string]Status {
	out := map[string]Status{}
	if r == nil {
		return out
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, t := range r.tunnels {
		out[id] = Status{Connected: true, LastHandshake: t.LastHandshake}
	}
	return out
}

func HTTPPath(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return "/api/v1/workers/tunnel"
	}
	u.Scheme = map[string]string{"http": "ws", "https": "wss"}[u.Scheme]
	if u.Scheme == "" {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/workers/tunnel"
	u.RawQuery = ""
	return u.String()
}
