package tasknotify

import (
	"sync"
	"time"
)

type TaskEvent struct {
	TaskID      string    `json:"task_id"`
	Repo        string    `json:"repo"`
	IssueNum    int       `json:"issue_num"`
	AgentName   string    `json:"agent_name"`
	Status      string    `json:"status"`
	ExitCode    int       `json:"exit_code"`
	DurationMS  int64     `json:"duration_ms"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

type Hub struct {
	mu          sync.Mutex
	nextID      int
	subscribers map[int]chan TaskEvent
}

func NewHub() *Hub {
	return &Hub{
		subscribers: make(map[int]chan TaskEvent),
	}
}

func (h *Hub) Subscribe() (int, <-chan TaskEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextID++
	id := h.nextID
	ch := make(chan TaskEvent, 1)
	h.subscribers[id] = ch
	return id, ch
}

func (h *Hub) Unsubscribe(id int) {
	h.mu.Lock()
	ch, ok := h.subscribers[id]
	if ok {
		delete(h.subscribers, id)
	}
	h.mu.Unlock()

	if ok {
		close(ch)
	}
}

func (h *Hub) Publish(event TaskEvent) {
	h.mu.Lock()
	subs := make([]chan TaskEvent, 0, len(h.subscribers))
	for _, ch := range h.subscribers {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	for _, ch := range subs {
		ch <- event
	}
}
