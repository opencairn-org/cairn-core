package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// startDLQListener subscribes to JetStream's MaxDeliveries advisory
// subject and captures every dead-letter event into Postgres for
// operator inspection + replay.
//
// Advisory subject:
//
//	$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.<stream>.<consumer>
//
// The advisory itself doesn't carry the dead payload — it's the
// observation that "this message hit MaxDeliver". For v1 we record
// metadata only. A future revision fetches the payload via
// js.GetMsg(stream, stream_seq) before recording.
//
// Gated on app.NATSBus + app.DeadLetters; returns (nil, nil) when
// either is missing.
func startDLQListener(
	ctx context.Context,
	app *App,
	logger *slog.Logger,
) (port.Subscription, error) {
	if app.NATSBus == nil || app.DeadLetters == nil {
		return nil, nil
	}
	log := logger.With("component", "dlq_listener")

	conn := app.NATSBus.Conn()
	if conn == nil {
		return nil, errors.New("dlq listener: NATS connection not available")
	}

	sub, err := conn.Subscribe(
		"$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>",
		func(msg *nats.Msg) {
			handleAdvisory(ctx, app, log, msg)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("subscribe DLQ advisory: %w", err)
	}
	log.Info("dlq listener active on $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>")

	return &dlqSubscription{sub: sub}, nil
}

type dlqSubscription struct{ sub *nats.Subscription }

func (s *dlqSubscription) Close(_ context.Context) error {
	if s.sub == nil {
		return nil
	}
	return s.sub.Drain()
}

// advisoryEvent is the JSON shape NATS publishes on the MAX_DELIVERIES
// advisory subject.
type advisoryEvent struct {
	Type       string    `json:"type"`
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	Stream     string    `json:"stream"`
	Consumer   string    `json:"consumer"`
	StreamSeq  uint64    `json:"stream_seq"`
	Deliveries int       `json:"deliveries"`
}

func handleAdvisory(ctx context.Context, app *App, log *slog.Logger, msg *nats.Msg) {
	var ev advisoryEvent
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		log.Warn("decode advisory failed", "subject", msg.Subject, "error", err)
		return
	}
	if ev.Stream == "" {
		return
	}

	// Payload-fetch via stream_seq is a future revision — for now we
	// record what the advisory gives us. The (stream, subject, msg_id)
	// triple is enough for operator triage; producers can re-derive
	// the body from their own logs.
	job := domain.DeadLetteredJob{
		Stream:         ev.Stream,
		Subject:        ev.Consumer, // best available; refined when we wire stream_seq fetch
		Consumer:       ev.Consumer,
		MsgID:          ev.ID,
		DeliveredCount: ev.Deliveries,
		LastError:      "exceeded MaxDeliver; advisory captured, payload pending fetch",
		FirstSeenAt:    ev.Timestamp,
		LastSeenAt:     ev.Timestamp,
		Headers:        map[string]string{},
	}
	if err := app.DeadLetters.Capture(ctx, job); err != nil {
		log.Warn("dlq capture failed",
			"stream", ev.Stream, "consumer", ev.Consumer, "error", err)
		return
	}
	log.Info("dead-letter recorded",
		"stream", ev.Stream, "consumer", ev.Consumer, "deliveries", ev.Deliveries)
	publishAdminEvent(app, domain.NewAdminEvent(domain.AdminEventJobDeadLettered, domain.AuditWarn, "admin.job_dead_lettered").
		AboutJob(ev.ID).With("stream", ev.Stream).With("consumer", ev.Consumer))
}

// publishAdminEvent fans ev out to open admin streams; a nil hub drops it.
func publishAdminEvent(app *App, ev domain.AdminEvent) {
	if app != nil && app.AdminEvents != nil {
		app.AdminEvents.Publish(ev)
	}
}
