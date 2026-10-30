package main

import (
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
)

func TestDegradedTransitions(t *testing.T) {
	workers := []liveWorker{{name: "a", provider: "strava", compatible: true}, {name: "b", provider: "strava", release: "v0.1.0"}}
	newly, next := degradedTransitions(nil, workers)
	if len(newly) != 1 || newly[0].name != "b" || !next["strava/b"] || next["strava/a"] {
		t.Fatalf("first tick: %+v %v", newly, next)
	}
	if again, _ := degradedTransitions(next, workers); len(again) != 0 {
		t.Fatalf("already reported: %+v", again)
	}
	workers[1].compatible = true
	if _, next = degradedTransitions(next, workers); len(next) != 0 {
		t.Fatalf("recovered worker dropped: %v", next)
	}
}

func TestHealthTransition(t *testing.T) {
	healthy := instance.Report{Overall: instance.Healthy, Database: instance.Component{State: instance.Healthy}}
	if _, ok := healthTransition(instance.Healthy, healthy); ok {
		t.Fatal("healthy → healthy is no event")
	}
	degraded := instance.Report{Overall: instance.Degraded, Storage: instance.Component{State: instance.Degraded, Detail: "object storage disabled (CAIRN_STORAGE_DRIVER=none)"}, JobsDLQ: 2}
	ev, ok := healthTransition(instance.Healthy, degraded)
	if !ok || ev.Type != domain.AdminEventSystemHealthDegraded || ev.Severity != domain.AuditWarn || ev.Params["storage"] == "" || ev.Params["dead_letters"] != "2" || ev.Params["database"] != "" {
		t.Fatalf("degraded: %v %+v", ok, ev)
	}
	if _, ok := healthTransition(instance.Degraded, degraded); ok {
		t.Fatal("unchanged state is no event")
	}
	unhealthy := instance.Report{Overall: instance.Unhealthy, Database: instance.Component{State: instance.Unhealthy, Detail: "ping failed"}}
	if ev, ok := healthTransition(instance.Degraded, unhealthy); !ok || ev.Severity != domain.AuditHigh || ev.Params["state"] != "unhealthy" {
		t.Fatalf("unhealthy: %v %+v", ok, ev)
	}
	if _, ok := healthTransition(instance.Unhealthy, healthy); ok {
		t.Fatal("recovery is logged, not published")
	}
}
