package nats

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// startServer runs an embedded JetStream-enabled NATS server for one test.
func startServer(t *testing.T, opts *server.Options) *server.Server {
	t.Helper()
	if opts == nil {
		opts = &server.Options{}
	}
	opts.Host, opts.Port, opts.JetStream = "127.0.0.1", -1, true
	// Config files under docker/ set http_port 8222, which the dev stack's
	// NATS already holds; no test uses monitoring, so don't bind it.
	opts.HTTPPort = 0
	opts.StoreDir, opts.NoLog, opts.NoSigs = t.TempDir(), true, true
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// newTestBus connects a Bus to a fresh server with a JOBS stream.
func newTestBus(t *testing.T) (*Bus, jetstream.JetStream) {
	t.Helper()
	s := startServer(t, nil)
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	bus, err := NewBusFromConn(nc, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bus.Close)
	if _, err := bus.js.CreateStream(context.Background(), jetstream.StreamConfig{Name: "JOBS", Subjects: []string{"jobs.>"}}); err != nil {
		t.Fatal(err)
	}
	return bus, bus.js
}

func streamMsgs(t *testing.T, js jetstream.JetStream, name string) uint64 {
	t.Helper()
	st, err := js.Stream(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return st.CachedInfo().State.Msgs
}

func TestBus_PublishToStream(t *testing.T) {
	bus, js := newTestBus(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "a"} {
		if err := bus.Publish(ctx, "jobs.x", id, []byte(id)); err != nil {
			t.Fatal(err)
		}
	}
	if n := streamMsgs(t, js, "JOBS"); n != 2 {
		t.Fatalf("stream msgs = %d; want 2 (dedup by msg id)", n)
	}
}

func TestBus_PublishOutsideStreamWithoutListener(t *testing.T) {
	bus, _ := newTestBus(t)
	if err := bus.Publish(context.Background(), "core.nobody", "", []byte("x")); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func TestBus_PublishToCoreSubscriberDeliversOnceWithoutWaiting(t *testing.T) {
	bus, _ := newTestBus(t)
	got := make(chan *nats.Msg, 4)
	sub, err := bus.conn.ChanSubscribe("core.listened", got)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := bus.conn.Flush(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := bus.Publish(context.Background(), "core.listened", "m1", []byte("hi")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("publish took %v; a core subscriber must not be awaited as a JetStream ack", d)
	}
	if m := <-got; string(m.Data) != "hi" || m.Header.Get(jetstream.MsgIDHeader) != "m1" {
		t.Fatalf("message = %q %v", m.Data, m.Header)
	}
	select {
	case m := <-got:
		t.Fatalf("delivered twice: %q", m.Data)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBus_PublishPicksUpStreamCreatedLater(t *testing.T) {
	bus, js := newTestBus(t)
	ctx := context.Background()
	bus.noStreamTTL = 0
	if err := bus.Publish(ctx, "late.x", "", []byte("core")); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "LATE", Subjects: []string{"late.>"}}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "late.x", "", []byte("stored")); err != nil {
		t.Fatal(err)
	}
	if n := streamMsgs(t, js, "LATE"); n != 1 {
		t.Fatalf("stream msgs = %d; want 1", n)
	}
}

func TestBus_PublishCachesRoutes(t *testing.T) {
	bus, _ := newTestBus(t)
	ctx := context.Background()
	for _, subj := range []string{"jobs.x", "core.x"} {
		if err := bus.Publish(ctx, subj, "", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if r := bus.routes["jobs.x"]; !r.stream {
		t.Errorf("jobs.x route = %+v; want stream", r)
	}
	if r, ok := bus.routes["core.x"]; !ok || r.stream || r.checked.IsZero() {
		t.Errorf("core.x route = %+v, %v; want cached miss", r, ok)
	}
}

func TestBus_PublishFallsBackToCoreWhenStreamDeleted(t *testing.T) {
	bus, js := newTestBus(t)
	ctx := context.Background()
	if err := bus.Publish(ctx, "jobs.gone", "", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := js.DeleteStream(ctx, "JOBS"); err != nil {
		t.Fatal(err)
	}
	// Cached stream is gone and nobody listens: no responders, so core publish takes over.
	if err := bus.Publish(ctx, "jobs.gone", "", []byte("b")); err != nil {
		t.Fatal(err)
	}
	bus.mu.Lock()
	_, cached := bus.routes["jobs.gone"]
	bus.mu.Unlock()
	if cached {
		t.Fatal("route for a deleted stream must be forgotten")
	}
}
