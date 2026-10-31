package main

import (
	"context"
	"log/slog"

	natsadapter "github.com/opencairn-org/cairn-core/internal/adapter/secondary/nats"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/pipeline"
)

// startFollowUpConsumer runs the follow-ups the pipeline publishes as
// cairn.events.activity.changed. A failed stage NAKs the event; the bus
// redelivers with back-off and MaxDeliver lands it in the DLQ.
//
// Gating: returns (nil, nil) when the NATS bus or the pipeline isn't wired.
func startFollowUpConsumer(ctx context.Context, app *App, logger *slog.Logger) (port.Subscription, error) {
	if app.NATSBus == nil || app.Pipeline == nil {
		return nil, nil
	}
	log := logger.With("component", "follow_up_consumer")
	log.Info("subscribing to " + pipeline.ActivityChangedSubject)
	return app.NATSBus.Subscribe(ctx, port.ConsumerConfig{
		Stream:  natsadapter.StreamEvents,
		Durable: "activity-follow-ups",
		Subject: pipeline.ActivityChangedSubject,
		// All: events published before the consumer first exists still run.
		DeliverPolicy: port.DeliverAll,
	}, followUpHandler(app.Pipeline, log))
}

func followUpHandler(p *pipeline.Pipeline, log *slog.Logger) port.MessageHandler {
	return func(ctx context.Context, msg port.Message) error {
		ev, err := pipeline.DecodeActivityChanged(msg.Body)
		if err != nil {
			log.Warn("decode activity-changed event failed", "error", err)
			return &port.TerminalError{Reason: "bad_payload", Cause: err}
		}
		ev.Redelivery = msg.DeliveryAttempt > 1
		return p.RunFollowUps(ctx, ev)
	}
}
