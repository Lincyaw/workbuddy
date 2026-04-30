package eventlog

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

type capturingPublisher struct {
	mu     sync.Mutex
	events []string
	count  atomic.Int64
}

func (c *capturingPublisher) PublishFromRaw(eventType, _ string, _ int, _ string) {
	c.mu.Lock()
	c.events = append(c.events, eventType)
	c.mu.Unlock()
	c.count.Add(1)
}

func TestLogPublishesToHookDispatcher(t *testing.T) {
	logger := newTestLogger(t)
	pub := &capturingPublisher{}
	logger.SetPublisher(pub)

	logger.Log(TypeAlert, "x/y", 1, map[string]string{"k": "v"})
	// hook_* events still write to SQLite but the publisher decides what to
	// do with them. Filtering happens inside the dispatcher.
	logger.Log("hook_overflow", "", 0, nil)

	if pub.count.Load() != 2 {
		t.Fatalf("expected 2 publishes, got %d", pub.count.Load())
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if pub.events[0] != TypeAlert {
		t.Fatalf("expected first event %s, got %s", TypeAlert, pub.events[0])
	}
}

func TestLogWithoutPublisherDoesNotPanic(t *testing.T) {
	logger := newTestLogger(t)
	logger.Log(TypeAlert, "x/y", 1, nil)
	logger.SetPublisher(nil)
	logger.Log(TypeAlert, "x/y", 1, nil)
}

func newTestLogger(t *testing.T) *EventLogger {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewEventLogger(s)
}

// TestWriteAndQuery verifies basic write + read round-trip with payload marshalling.
func TestWriteAndQuery(t *testing.T) {
	logger := newTestLogger(t)

	type payload struct {
		Labels []string `json:"labels"`
	}
	logger.Log(TypeTransition, "owner/repo", 42, payload{Labels: []string{"agent:dev"}})
	logger.Log(TypeDispatch, "owner/repo", 42, map[string]string{"agent": "dev"})
	logger.Log(TypePoll, "owner/other", 0, nil)

	// Query all
	events, err := logger.Query(EventFilter{})
	if err != nil {
		t.Fatalf("Query all: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// Verify first event payload is valid JSON.
	ev := events[0]
	if ev.Type != TypeTransition {
		t.Errorf("expected type %q, got %q", TypeTransition, ev.Type)
	}
	if ev.Repo != "owner/repo" {
		t.Errorf("expected repo %q, got %q", "owner/repo", ev.Repo)
	}
	if ev.IssueNum != 42 {
		t.Errorf("expected issue_num 42, got %d", ev.IssueNum)
	}
	if ev.Payload == "" {
		t.Error("expected non-empty payload")
	}
}

// TestQueryWithFilters verifies filtering by type, repo, issue, and time range.
func TestQueryWithFilters(t *testing.T) {
	logger := newTestLogger(t)

	logger.Log(TypeTransition, "owner/repo", 1, "t1")
	logger.Log(TypeDispatch, "owner/repo", 2, "d1")
	logger.Log(TypeError, "owner/other", 1, "e1")
	logger.Log(TypeCompleted, "owner/repo", 1, "c1")

	// Filter by type.
	events, err := logger.Query(EventFilter{Type: TypeTransition})
	if err != nil {
		t.Fatalf("filter by type: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 transition event, got %d", len(events))
	}

	// Filter by repo.
	events, err = logger.Query(EventFilter{Repo: "owner/other"})
	if err != nil {
		t.Fatalf("filter by repo: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event for owner/other, got %d", len(events))
	}

	// Filter by issue number.
	events, err = logger.Query(EventFilter{Repo: "owner/repo", IssueNum: 1})
	if err != nil {
		t.Fatalf("filter by issue: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events for issue 1, got %d", len(events))
	}

	// Filter by time range — Since in the past should return all, future should return none.
	past := time.Now().Add(-1 * time.Hour)
	events, err = logger.Query(EventFilter{Since: &past})
	if err != nil {
		t.Fatalf("filter by since: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 events since past, got %d", len(events))
	}

	future := time.Now().Add(1 * time.Hour)
	events, err = logger.Query(EventFilter{Since: &future})
	if err != nil {
		t.Fatalf("filter by future since: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events since future, got %d", len(events))
	}
}

// TestConcurrentWrites verifies that concurrent Log calls don't panic or lose data.
func TestConcurrentWrites(t *testing.T) {
	logger := newTestLogger(t)

	const goroutines = 20
	const eventsPerGoroutine = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(g int) {
			defer wg.Done()
			for j := 0; j < eventsPerGoroutine; j++ {
				logger.Log(TypePoll, "owner/repo", g, map[string]int{"iter": j})
			}
		}(i)
	}
	wg.Wait()

	events, err := logger.Query(EventFilter{})
	if err != nil {
		t.Fatalf("Query after concurrent writes: %v", err)
	}
	expected := goroutines * eventsPerGoroutine
	if len(events) != expected {
		t.Fatalf("expected %d events, got %d", expected, len(events))
	}
}

func TestTypeRateLimitInAllEventTypes(t *testing.T) {
	found := false
	for _, t := range AllEventTypes {
		if t == TypeRateLimit {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("TypeRateLimit is missing from AllEventTypes")
	}
}

func TestTypeReportOverflowInAllEventTypes(t *testing.T) {
	found := false
	for _, t := range AllEventTypes {
		if t == TypeReportOverflow {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("TypeReportOverflow is missing from AllEventTypes")
	}
}

func TestWriteRateLimitEvent(t *testing.T) {
	logger := newTestLogger(t)
	logger.Log(TypeRateLimit, "owner/repo", 17, map[string]string{"source": "poller"})

	events, err := logger.Query(EventFilter{Type: TypeRateLimit, Repo: "owner/repo", IssueNum: 17})
	if err != nil {
		t.Fatalf("Query rate limit event: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 rate_limit event, got %d", len(events))
	}
	if events[0].Type != TypeRateLimit {
		t.Fatalf("expected type %q got %q", TypeRateLimit, events[0].Type)
	}
}

// TestHealthHappyPath verifies successful writes never mark the logger
// degraded.
func TestHealthHappyPath(t *testing.T) {
	logger := newTestLogger(t)

	logger.Log(TypePoll, "owner/repo", 1, nil)
	logger.Log(TypeDispatch, "owner/repo", 1, map[string]string{"agent": "dev"})

	h := logger.Health()
	if h.Degraded {
		t.Fatalf("expected Degraded=false after successful writes, got %+v", h)
	}
	if h.WriteFailures != 0 {
		t.Fatalf("expected WriteFailures=0, got %d", h.WriteFailures)
	}
	if h.LastError != "" {
		t.Fatalf("expected empty LastError, got %q", h.LastError)
	}
	if !h.LastFailureAt.IsZero() {
		t.Fatalf("expected zero LastFailureAt, got %v", h.LastFailureAt)
	}
}

// TestHealthDegradedOnWriteFailure verifies that a failed write marks the
// logger degraded, increments the counter, and captures the last error — but
// Log() itself still returns cleanly (best-effort contract preserved).
func TestHealthDegradedOnWriteFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}
	logger := NewEventLogger(s)

	// Healthy baseline.
	logger.Log(TypePoll, "owner/repo", 1, nil)
	if h := logger.Health(); h.Degraded {
		t.Fatalf("expected healthy baseline, got %+v", h)
	}

	// Close the store so subsequent InsertEvent calls fail.
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	before := time.Now().UTC()
	// These must not panic or block despite the closed store.
	logger.Log(TypePoll, "owner/repo", 2, nil)
	logger.Log(TypeDispatch, "owner/repo", 3, nil)

	h := logger.Health()
	if !h.Degraded {
		t.Fatalf("expected Degraded=true after failed writes, got %+v", h)
	}
	if h.WriteFailures != 2 {
		t.Fatalf("expected WriteFailures=2, got %d", h.WriteFailures)
	}
	if h.LastError == "" {
		t.Fatalf("expected non-empty LastError")
	}
	if h.LastFailureAt.Before(before) {
		t.Fatalf("expected LastFailureAt >= %v, got %v", before, h.LastFailureAt)
	}
}

// TestHealthConcurrentSafe verifies Health() is safe to call while Log()
// runs from multiple goroutines. Run under `go test -race`.
func TestHealthConcurrentSafe(t *testing.T) {
	logger := newTestLogger(t)

	const writers = 4
	const readers = 4
	const iterations = 20

	stop := make(chan struct{})

	var readerWG sync.WaitGroup
	readerWG.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = logger.Health()
				}
			}
		}()
	}

	var writerWG sync.WaitGroup
	writerWG.Add(writers)
	for i := 0; i < writers; i++ {
		go func(g int) {
			defer writerWG.Done()
			for j := 0; j < iterations; j++ {
				logger.Log(TypePoll, "owner/repo", g, map[string]int{"iter": j})
			}
		}(i)
	}

	writerWG.Wait()
	close(stop)
	readerWG.Wait()

	// The primary invariant is that Health() and Log() race cleanly under
	// `-race`. The SQLite backend can occasionally return SQLITE_BUSY under
	// heavy concurrent writes; if that happens the logger correctly marks
	// itself degraded. Either outcome is acceptable for this test — we only
	// assert the numbers are self-consistent.
	h := logger.Health()
	if h.Degraded && h.WriteFailures == 0 {
		t.Fatalf("Degraded=true but WriteFailures=0: %+v", h)
	}
	if !h.Degraded && h.WriteFailures != 0 {
		t.Fatalf("Degraded=false but WriteFailures=%d: %+v", h.WriteFailures, h)
	}
}

func TestWriteReportOverflowEvent(t *testing.T) {
	logger := newTestLogger(t)
	logger.Log(TypeReportOverflow, "owner/repo", 17, map[string]any{
		"body_bytes": 70001,
		"committed":  true,
	})

	events, err := logger.Query(EventFilter{Type: TypeReportOverflow, Repo: "owner/repo", IssueNum: 17})
	if err != nil {
		t.Fatalf("Query report overflow event: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 report_overflow event, got %d", len(events))
	}
	if events[0].Type != TypeReportOverflow {
		t.Fatalf("expected type %q got %q", TypeReportOverflow, events[0].Type)
	}
}
