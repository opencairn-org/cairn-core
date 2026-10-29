package livefeed

import "testing"

func TestHubRoutesByKeyAndDropsWhenFull(t *testing.T) {
	h := New(func(s string) string { return s[:1] })
	a, cancelA := h.Subscribe("a")
	defer cancelA()
	b, cancelB := h.Subscribe("b")
	h.Publish("a1")
	h.Publish("b1")
	if got := <-a; got != "a1" {
		t.Fatalf("a got %q", got)
	}
	if got := <-b; got != "b1" {
		t.Fatalf("b got %q", got)
	}
	cancelB()
	cancelB() // idempotent
	if _, ok := <-b; ok {
		t.Fatal("cancel must close the channel")
	}
	if h.Subscribers("b") != 0 || h.Subscribers("a") != 1 {
		t.Fatal("subscriber counts")
	}
	for i := 0; i < subscriberBuffer+5; i++ {
		h.Publish("a-flood") // never blocks
	}
	if len(a) != subscriberBuffer {
		t.Fatalf("buffer %d", len(a))
	}
}
