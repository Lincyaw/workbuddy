package tasknotify

import (
	"testing"
	"time"
)

func TestHubPublishToUnconsumedSubscriberDoesNotBlock(t *testing.T) {
	hub := NewHub()
	hub.Subscribe()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			hub.Publish(TaskEvent{TaskID: "task", Repo: "owner/repo", IssueNum: i})
		}
		close(done)
	}()

	select {
	case <-done:
		// non-blocking
	case <-time.After(100 * time.Millisecond):
		t.Fatal("hub.Publish blocked when subscriber was not consuming")
	}
}

func TestHubPublishesToMultipleSubscribersAndDropsFullChannel(t *testing.T) {
	hub := NewHub()
	_, ch1 := hub.Subscribe()
	_, ch2 := hub.Subscribe()

	event := TaskEvent{
		TaskID:    "task-id",
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
		Status:    "completed",
	}
	hub.Publish(event)
	hub.Publish(event)

	select {
	case got := <-ch1:
		if got.IssueNum != 42 {
			t.Fatalf("ch1 issue=%d, want 42", got.IssueNum)
		}
	default:
		t.Fatal("expected event on ch1")
	}

	select {
	case got := <-ch2:
		if got.IssueNum != 42 {
			t.Fatalf("ch2 issue=%d, want 42", got.IssueNum)
		}
	default:
		t.Fatal("expected event on ch2")
	}

	select {
	case <-ch1:
		t.Fatal("ch1 should not queue extra events while full")
	default:
	}
	select {
	case <-ch2:
		t.Fatal("ch2 should not queue extra events while full")
	default:
	}
}
