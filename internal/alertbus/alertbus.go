package alertbus

import (
	"sync"
)

// Severity is the high-level urgency label for outbound alerts.
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warning"
	SeverityError Severity = "error"
)

const (
	KindCycleLimitReached       = "cycle_limit_reached"
	KindTransitionToFailed      = "transition_to_failed"
	KindStuckDetected          = "stuck_detected"
	KindDependencyCycleDetected = "dependency_cycle_detected"
	KindOrphanedTask           = "orphaned_task"
	KindAgentExitNonZero       = "agent_exit_non_zero"
	KindRepeatedFailure        = "repeated_failure"
	KindTaskCompletedSuccess    = "task_completed"
	KindDispatchBlocked         = "dispatch_blocked"
)

// AlertEvent is the payload emitted by system components for notification.
type AlertEvent struct {
	Kind       string    `json:"kind"`
	Severity   Severity  `json:"severity"`
	Repo       string    `json:"repo"`
	IssueNum   int       `json:"issue_num"`
	AgentName  string    `json:"agent_name"`
	Timestamp  int64     `json:"timestamp"`
	Payload    map[string]any
}

// Bus provides a buffered pub/sub mechanism for alert events.
type Bus struct {
	mu          sync.Mutex
	nextID      int
	buffer      int
	subscribers map[int]chan AlertEvent
}

// NewBus creates a new alert bus with a per-subscriber buffer size.
func NewBus(buffer int) *Bus {
	if buffer <= 0 {
		buffer = 64
	}
	return &Bus{
		buffer:      buffer,
		subscribers: make(map[int]chan AlertEvent),
	}
}

// Subscribe registers a consumer channel.
func (b *Bus) Subscribe() (int, <-chan AlertEvent) {
	ch := make(chan AlertEvent, b.buffer)

	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subscribers[id] = ch
	b.mu.Unlock()
	return id, ch
}

// Unsubscribe removes a consumer channel.
func (b *Bus) Unsubscribe(id int) {
	b.mu.Lock()
	ch, ok := b.subscribers[id]
	if ok {
		delete(b.subscribers, id)
	}
	b.mu.Unlock()
	if ok {
		close(ch)
	}
}

// Publish pushes an alert event into each subscriber queue without blocking.
func (b *Bus) Publish(event AlertEvent) {
	b.mu.Lock()
	subscribers := make([]chan AlertEvent, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subscribers = append(subscribers, ch)
	}
	b.mu.Unlock()

	for _, ch := range subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}
