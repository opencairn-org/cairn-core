package instance

import (
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestHealthTransition(t *testing.T) {
	healthy := Report{Overall: Healthy, Database: Component{State: Healthy}}
	if _, ok := HealthTransition(Healthy, healthy); ok {
		t.Fatal("healthy → healthy is no event")
	}
	degraded := Report{Overall: Degraded, Storage: Component{State: Degraded, Detail: "object storage disabled (CAIRN_STORAGE_DRIVER=none)"}, JobsDLQ: 2}
	ev, ok := HealthTransition(Healthy, degraded)
	if !ok || ev.Type != domain.AdminEventSystemHealthDegraded || ev.Severity != domain.AuditWarn || ev.Params["storage"] == "" || ev.Params["dead_letters"] != "2" || ev.Params["database"] != "" {
		t.Fatalf("degraded: %v %+v", ok, ev)
	}
	if _, ok := HealthTransition(Degraded, degraded); ok {
		t.Fatal("unchanged state is no event")
	}
	unhealthy := Report{Overall: Unhealthy, Database: Component{State: Unhealthy, Detail: "ping failed"}, Workers: WorkerCounts{Offline: 1}}
	if ev, ok := HealthTransition(Degraded, unhealthy); !ok || ev.Severity != domain.AuditHigh || ev.Params["state"] != "unhealthy" || ev.Params["workers"] == "" {
		t.Fatalf("unhealthy: %v %+v", ok, ev)
	}
	if _, ok := HealthTransition(Unhealthy, healthy); ok {
		t.Fatal("recovery is logged, not published")
	}
}
