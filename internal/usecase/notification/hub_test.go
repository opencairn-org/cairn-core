package notification

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type memNotifs struct {
	port.NotificationRepo
	saved int
}

func (m *memNotifs) SaveNotifications(_ context.Context, n []domain.Notification) error {
	m.saved += len(n)
	return nil
}

func TestHubDeliversPerUserAndDropsSlow(t *testing.T) {
	hub := NewHub()
	alice, bob := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	chA, cancelA := hub.Subscribe(alice)
	defer cancelA()
	chB, cancelB := hub.Subscribe(bob)

	repo := NewBroadcastRepo(&memNotifs{}, hub)
	if err := repo.SaveNotifications(context.Background(), []domain.Notification{{UserID: alice, DedupKey: "a1"}, {UserID: bob, DedupKey: "b1"}, {UserID: alice, DedupKey: "a2"}}); err != nil {
		t.Fatal(err)
	}
	got := <-chA
	if len(got) != 2 || got[0].DedupKey != "a1" || got[1].DedupKey != "a2" {
		t.Fatalf("alice got %v", got)
	}
	if got := <-chB; len(got) != 1 || got[0].DedupKey != "b1" {
		t.Fatalf("bob got %v", got)
	}
	// Unsubscribe closes the channel and stops delivery.
	cancelB()
	cancelB()
	if _, open := <-chB; open {
		t.Fatal("channel must be closed after cancel")
	}
	if hub.Subscribers(bob) != 0 || hub.Subscribers(alice) != 1 {
		t.Fatal("subscriber bookkeeping wrong")
	}
	// A slow subscriber keeps the writer from blocking: overflow is dropped.
	for i := range subscriberBuffer + 5 {
		hub.Publish([]domain.Notification{{UserID: alice, DedupKey: "x" + string(rune('0'+i%10))}})
	}
	n := 0
	for len(chA) > 0 {
		<-chA
		n++
	}
	if n != subscriberBuffer {
		t.Fatalf("buffered %d, want %d", n, subscriberBuffer)
	}
}
