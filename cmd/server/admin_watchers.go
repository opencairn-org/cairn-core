package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/opencairn-org/cairn-core/internal/usecase/fleet"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
)

// Operator event sources beyond the presence watcher: worker releases that
// fall out of the core's compatibility window and the instance health probe
// leaving "healthy". Both publish on the transition only.

// noteDegraded publishes one event per worker entering the degraded state.
func (w *workerPresenceWatcher) noteDegraded(ctx context.Context) {
	newly, next := fleet.DegradedTransitions(w.degraded, domainWorkers(currentWorkers(ctx, w.app, w.log)))
	w.degraded = next
	for _, lw := range newly {
		w.log.Warn("worker release incompatible with core", "worker", lw.Name, "provider", lw.Provider, "release", lw.Release)
		publishAdminEvent(w.app, fleet.DegradedEvent(lw, Version))
	}
}

// runHealthWatcher probes the instance health every minute and publishes
// the transitions out of "healthy"; recoveries are logged only.
func runHealthWatcher(ctx context.Context, logger *slog.Logger, app *App) {
	log := logger.With("component", "health_watcher")
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	prev := instance.Healthy
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rep := app.SystemHealth.Check(ctx)
			if ev, ok := instance.HealthTransition(prev, rep); ok {
				log.Warn("instance health changed", "state", rep.Overall, "details", ev.Params)
				publishAdminEvent(app, ev)
			} else if rep.Overall == instance.Healthy && prev != instance.Healthy {
				log.Info("instance health recovered")
			}
			prev = rep.Overall
		}
	}
}
