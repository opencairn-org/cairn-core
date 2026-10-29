package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
)

// Operator event sources beyond the presence watcher: worker releases that
// fall out of the core's compatibility window and the instance health probe
// leaving "healthy". Both publish on the transition only.

// degradedTransitions returns the live workers that just became incompatible
// and the set to carry into the next tick.
func degradedTransitions(prev map[string]bool, workers []liveWorker) (newly []liveWorker, next map[string]bool) {
	next = map[string]bool{}
	for _, w := range workers {
		if w.compatible {
			continue
		}
		key := w.provider + "/" + w.name
		next[key] = true
		if !prev[key] {
			newly = append(newly, w)
		}
	}
	return newly, next
}

// noteDegraded publishes one event per worker entering the degraded state.
func (w *workerPresenceWatcher) noteDegraded(ctx context.Context) {
	newly, next := degradedTransitions(w.degraded, currentWorkers(ctx, w.app, w.log))
	w.degraded = next
	for _, lw := range newly {
		w.log.Warn("worker release incompatible with core", "worker", lw.name, "provider", lw.provider, "release", lw.release)
		publishAdminEvent(w.app, domain.NewAdminEvent(domain.AdminEventWorkerDegraded, domain.AuditWarn, "admin.worker_degraded").
			AboutWorker(lw.name).With("provider", lw.provider).With("release", lw.release).With("core_release", Version))
	}
}

// healthTransition builds the event for a probe result; ok is false when the
// state is unchanged or back to healthy.
func healthTransition(prev instance.HealthState, rep instance.Report) (domain.AdminEvent, bool) {
	if rep.Overall == prev || rep.Overall == instance.Healthy {
		return domain.AdminEvent{}, false
	}
	severity := domain.AuditWarn
	if rep.Overall == instance.Unhealthy {
		severity = domain.AuditHigh
	}
	ev := domain.NewAdminEvent(domain.AdminEventSystemHealthDegraded, severity, "admin.system_health_degraded").With("state", string(rep.Overall))
	for name, c := range map[string]instance.Component{"database": rep.Database, "queue": rep.Queue, "storage": rep.Storage, "email": rep.Email} {
		if c.State != "" && c.State != instance.Healthy {
			ev = ev.With(name, string(c.State)+": "+c.Detail)
		}
	}
	if rep.Workers.Healthy == 0 && (rep.Workers.Degraded > 0 || rep.Workers.Offline > 0) {
		ev = ev.With("workers", fmt.Sprintf("healthy %d, degraded %d, offline %d", rep.Workers.Healthy, rep.Workers.Degraded, rep.Workers.Offline))
	}
	if rep.JobsDLQ > 0 {
		ev = ev.With("dead_letters", fmt.Sprint(rep.JobsDLQ))
	}
	return ev, true
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
			if ev, ok := healthTransition(prev, rep); ok {
				log.Warn("instance health changed", "state", rep.Overall, "details", ev.Params)
				publishAdminEvent(app, ev)
			} else if rep.Overall == instance.Healthy && prev != instance.Healthy {
				log.Info("instance health recovered")
			}
			prev = rep.Overall
		}
	}
}
