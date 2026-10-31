package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Dead-letter advisories. MAX_DELIVERIES covers every consumer; a Term is
// only a dead letter on the streams core itself consumes (workers term jobs
// on purpose, e.g. a parser hand-back).
const (
	advisoryMaxDeliveries = "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>"
	advisoryTerminated    = "$JS.EVENT.ADVISORY.CONSUMER.MSG_TERMINATED.>"
)

var dlqTermStreams = map[string]bool{"CAIRN_RESULTS": true, "CAIRN_EVENTS": true}

// startDLQListener subscribes to JetStream's dead-letter advisories and
// captures each event into Postgres for operator inspection + replay. The
// payload is fetched by stream sequence while the stream still holds it.
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

	subs := make([]*nats.Subscription, 0, 2)
	for _, subject := range []string{advisoryMaxDeliveries, advisoryTerminated} {
		sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
			handleAdvisory(ctx, app, log, msg)
		})
		if err != nil {
			for _, s := range subs {
				_ = s.Unsubscribe()
			}
			return nil, fmt.Errorf("subscribe DLQ advisory %s: %w", subject, err)
		}
		subs = append(subs, sub)
	}
	log.Info("dlq listener active", "subjects", []string{advisoryMaxDeliveries, advisoryTerminated})

	return &dlqSubscription{subs: subs}, nil
}

type dlqSubscription struct{ subs []*nats.Subscription }

func (s *dlqSubscription) Close(_ context.Context) error {
	var errs []error
	for _, sub := range s.subs {
		errs = append(errs, sub.Drain())
	}
	return errors.Join(errs...)
}

// advisoryEvent is the JSON shape of the MAX_DELIVERIES and MSG_TERMINATED
// advisories (reason only on the latter).
type advisoryEvent struct {
	Type       string    `json:"type"`
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	Stream     string    `json:"stream"`
	Consumer   string    `json:"consumer"`
	StreamSeq  uint64    `json:"stream_seq"`
	Deliveries int       `json:"deliveries"`
	Reason     string    `json:"reason"`
}

// isTerminated reports whether the advisory was a Term rather than MaxDeliver.
func (ev advisoryEvent) isTerminated() bool { return strings.HasSuffix(ev.Type, ".terminated") }

func handleAdvisory(ctx context.Context, app *App, log *slog.Logger, msg *nats.Msg) {
	var ev advisoryEvent
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		log.Warn("decode advisory failed", "subject", msg.Subject, "error", err)
		return
	}
	if ev.Stream == "" || (ev.isTerminated() && !dlqTermStreams[ev.Stream]) {
		return
	}
	job := deadLetterFromAdvisory(ctx, app, log, ev)
	if err := app.DeadLetters.Capture(ctx, job); err != nil {
		log.Warn("dlq capture failed",
			"stream", ev.Stream, "consumer", ev.Consumer, "error", err)
		return
	}
	log.Info("dead-letter recorded",
		"stream", ev.Stream, "consumer", ev.Consumer, "deliveries", ev.Deliveries, "reason", job.LastError)
	publishAdminEvent(app, domain.NewAdminEvent(domain.AdminEventJobDeadLettered, domain.AuditWarn, "admin.job_dead_lettered").
		AboutJob(job.MsgID).With("stream", ev.Stream).With("consumer", ev.Consumer))
}

// deadLetterFromAdvisory builds the DLQ row, with the payload when the stream
// still holds the message (a MaxDeliver one usually does; a Term'd one on an
// interest stream is usually gone).
func deadLetterFromAdvisory(ctx context.Context, app *App, log *slog.Logger, ev advisoryEvent) domain.DeadLetteredJob {
	job := domain.DeadLetteredJob{
		Stream:         ev.Stream,
		Subject:        ev.Consumer,
		Consumer:       ev.Consumer,
		MsgID:          ev.ID,
		DeliveredCount: ev.Deliveries,
		LastError:      "exceeded MaxDeliver",
		FirstSeenAt:    ev.Timestamp,
		LastSeenAt:     ev.Timestamp,
		Headers:        map[string]string{},
	}
	if ev.isTerminated() {
		job.LastError = "terminated"
		if ev.Reason != "" {
			job.LastError = "terminated: " + ev.Reason
		}
	}
	if ev.StreamSeq == 0 {
		return job
	}
	subject, data, headers, err := app.NATSBus.StoredMsg(ctx, ev.Stream, ev.StreamSeq)
	if err != nil {
		log.Debug("dlq payload not available", "stream", ev.Stream, "seq", ev.StreamSeq, "error", err)
		job.LastError += "; payload no longer in the stream"
		return job
	}
	job.Subject, job.Payload, job.Headers = subject, data, headers
	if id := headers[nats.MsgIdHdr]; id != "" {
		job.MsgID = id
	}
	return job
}

// publishAdminEvent fans ev out to open admin streams; a nil hub drops it.
func publishAdminEvent(app *App, ev domain.AdminEvent) {
	if app != nil && app.AdminEvents != nil {
		app.AdminEvents.Publish(ev)
	}
}
