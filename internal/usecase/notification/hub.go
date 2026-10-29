package notification

import (
	"context"
	"sync"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Hub fans freshly saved notifications out to in-process subscribers (the
// StreamNotifications RPC). It is best-effort live delivery: the durable
// feed stays in the repo, so a slow subscriber is dropped from a batch
// rather than blocking the writer.
type Hub struct {
	mu   sync.Mutex
	subs map[domain.UserID]map[*subscriber]struct{}
}

type subscriber struct {
	ch chan []domain.Notification
}

// subscriberBuffer bounds how many batches a slow stream may fall behind.
const subscriberBuffer = 16

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[domain.UserID]map[*subscriber]struct{}{}} }

// Subscribe returns a channel of notification batches for userID and the
// function that ends the subscription (idempotent).
func (h *Hub) Subscribe(userID domain.UserID) (<-chan []domain.Notification, func()) {
	s := &subscriber{ch: make(chan []domain.Notification, subscriberBuffer)}
	h.mu.Lock()
	if h.subs[userID] == nil {
		h.subs[userID] = map[*subscriber]struct{}{}
	}
	h.subs[userID][s] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs[userID], s)
			if len(h.subs[userID]) == 0 {
				delete(h.subs, userID)
			}
			h.mu.Unlock()
			close(s.ch)
		})
	}
}

// Publish delivers notifs to the subscribers of each recipient. A full
// subscriber buffer drops that batch for that subscriber.
func (h *Hub) Publish(notifs []domain.Notification) {
	byUser := map[domain.UserID][]domain.Notification{}
	for _, n := range notifs {
		byUser[n.UserID] = append(byUser[n.UserID], n)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for uid, batch := range byUser {
		for s := range h.subs[uid] {
			select {
			case s.ch <- batch:
			default:
			}
		}
	}
}

// Subscribers reports how many streams are open for userID (tests, metrics).
func (h *Hub) Subscribers(userID domain.UserID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[userID])
}

// BroadcastRepo is a port.NotificationRepo that publishes every saved batch
// to a Hub, so every producer (PR dispatch, gear decisions, re-auth, worker
// offline) feeds live streams without knowing about them.
type BroadcastRepo struct {
	port.NotificationRepo
	hub *Hub
}

// NewBroadcastRepo wraps inner.
func NewBroadcastRepo(inner port.NotificationRepo, hub *Hub) *BroadcastRepo {
	return &BroadcastRepo{NotificationRepo: inner, hub: hub}
}

// SaveNotifications saves, then publishes the batch as saved.
func (r *BroadcastRepo) SaveNotifications(ctx context.Context, notifs []domain.Notification) error {
	if err := r.NotificationRepo.SaveNotifications(ctx, notifs); err != nil {
		return err
	}
	r.hub.Publish(notifs)
	return nil
}
