package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	natsadapter "github.com/opencairn-org/cairn-core/internal/adapter/secondary/nats"
)

func dlqTestBus(t *testing.T) (*natsadapter.Bus, jetstream.JetStream) {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(s.Shutdown)
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	bus, err := natsadapter.NewBusFromConn(nc, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bus.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return bus, js
}

func TestDeadLetterFromAdvisory(t *testing.T) {
	bus, js := dlqTestBus(t)
	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "CAIRN_RESULTS", Subjects: []string{"cairn.results.>"}}); err != nil {
		t.Fatal(err)
	}
	ack, err := js.PublishMsg(ctx, &nats.Msg{Subject: "cairn.results.fetch_source.strava.w", Data: []byte(`{"x":1}`), Header: nats.Header{nats.MsgIdHdr: []string{"m-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{NATSBus: bus}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	maxed := advisoryEvent{Type: "io.nats.jetstream.advisory.v1.max_deliver", Stream: "CAIRN_RESULTS", Consumer: "ingest-router", StreamSeq: ack.Sequence, Deliveries: 5}
	job := deadLetterFromAdvisory(ctx, app, log, maxed)
	if string(job.Payload) != `{"x":1}` || job.Subject != "cairn.results.fetch_source.strava.w" || job.MsgID != "m-1" || job.LastError != "exceeded MaxDeliver" {
		t.Fatalf("max-deliver row: %+v", job)
	}

	termed := advisoryEvent{Type: "io.nats.jetstream.advisory.v1.terminated", Stream: "CAIRN_RESULTS", Consumer: "ingest-router", StreamSeq: ack.Sequence + 99, Reason: "bad_payload"}
	if !termed.isTerminated() || maxed.isTerminated() {
		t.Fatal("isTerminated")
	}
	job = deadLetterFromAdvisory(ctx, app, log, termed)
	if job.LastError != "terminated: bad_payload; payload no longer in the stream" || job.Payload != nil {
		t.Fatalf("terminated row: %+v", job)
	}
}
