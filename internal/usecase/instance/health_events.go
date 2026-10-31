package instance

import (
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// HealthTransition builds the operator event for a probe result; ok is false
// when the state is unchanged or back to healthy (recoveries are only logged).
func HealthTransition(prev HealthState, rep Report) (domain.AdminEvent, bool) {
	if rep.Overall == prev || rep.Overall == Healthy {
		return domain.AdminEvent{}, false
	}
	severity := domain.AuditWarn
	if rep.Overall == Unhealthy {
		severity = domain.AuditHigh
	}
	ev := domain.NewAdminEvent(domain.AdminEventSystemHealthDegraded, severity, "admin.system_health_degraded").With("state", string(rep.Overall))
	for name, c := range map[string]Component{"database": rep.Database, "queue": rep.Queue, "storage": rep.Storage, "email": rep.Email} {
		if c.State != "" && c.State != Healthy {
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
