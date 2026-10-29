// Package livefeed is the in-process fan-out behind the server-streaming
// RPCs: producers publish, open streams subscribe by key (a user, or the
// admin channel). Delivery is best-effort live; durable state stays in the
// repos, so a slow subscriber drops items rather than blocking a writer.
package livefeed

import "sync"

// Hub fans items of type T out to the subscribers of their key.
type Hub[K comparable, T any] struct {
	keyOf func(T) K
	mu    sync.Mutex
	subs  map[K]map[*subscriber[T]]struct{}
}

type subscriber[T any] struct {
	ch chan T
}

// subscriberBuffer bounds how far a slow stream may fall behind.
const subscriberBuffer = 64

// New returns an empty hub routing each item by keyOf.
func New[K comparable, T any](keyOf func(T) K) *Hub[K, T] {
	return &Hub[K, T]{keyOf: keyOf, subs: map[K]map[*subscriber[T]]struct{}{}}
}

// Subscribe returns a channel of items for key and the function that ends
// the subscription (idempotent; closes the channel).
func (h *Hub[K, T]) Subscribe(key K) (<-chan T, func()) {
	s := &subscriber[T]{ch: make(chan T, subscriberBuffer)}
	h.mu.Lock()
	if h.subs[key] == nil {
		h.subs[key] = map[*subscriber[T]]struct{}{}
	}
	h.subs[key][s] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs[key], s)
			if len(h.subs[key]) == 0 {
				delete(h.subs, key)
			}
			h.mu.Unlock()
			close(s.ch)
		})
	}
}

// Publish delivers item to every subscriber of its key; a full buffer drops it.
func (h *Hub[K, T]) Publish(item T) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[h.keyOf(item)] {
		select {
		case s.ch <- item:
		default:
		}
	}
}

// Subscribers reports how many streams are open for key.
func (h *Hub[K, T]) Subscribers(key K) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[key])
}
