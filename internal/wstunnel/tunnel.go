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

	"github.com/coder/websocket"
)

const (
	FrameRequest  = "request"
	FrameResponse = "response"
	FrameClose    = "close"
)

var ErrClosed = errors.New("wstunnel: closed")

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
}

// MaxFrameBytes caps a single tunnel WebSocket message at 64 MiB. The
// default nhooyr/websocket limit is 32 KiB, far too small for session
// listings or events responses that are buffered into one frame today
// (REQ-132 will introduce body streaming).
const MaxFrameBytes = 64 << 20

func NewEndpoint(conn *websocket.Conn) *Endpoint {
	if conn != nil {
		conn.SetReadLimit(MaxFrameBytes)
	}
	return &Endpoint{
		conn:     conn,
		streams:  make(map[string]chan Frame),
		requests: make(chan Frame, 64),
		closed:   make(chan struct{}),
	}
}

func (e *Endpoint) Run(ctx context.Context) error {
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

func (e *Endpoint) send(ctx context.Context, frame Frame) error {
	select {
	case <-e.closed:
		return e.closeErr
	default:
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
