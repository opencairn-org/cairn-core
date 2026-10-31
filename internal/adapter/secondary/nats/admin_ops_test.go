package nats

import (
	"context"
	"testing"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
)

func TestPresenceHeartbeatToDomain(t *testing.T) {
	seen := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	hb := presenceHeartbeat{WorkerName: "w1", Provider: "strava", Release: "v1.2.3", Package: "pkg",
		Capabilities: map[string]capability.Capability{"activities": {Read: true, Backfill: true}}}
	got := hb.toDomain("v1.2.0", seen)
	if got.WorkerName != "w1" || !got.Compatible || !got.Known || !got.LastSeen.Equal(seen) ||
		!got.Capabilities["activities"].Read || !got.Capabilities["activities"].Backfill {
		t.Fatalf("heartbeat = %+v", got)
	}
}

func TestNilBusOps(t *testing.T) {
	var b *Bus
	if ok, detail := b.Health(); ok || detail != "message bus not configured" {
		t.Fatalf("health = %v %q", ok, detail)
	}
	if hbs := b.WorkerHeartbeats(context.Background(), "v1.0.0", time.Minute, nil); hbs == nil || len(hbs) != 0 {
		t.Fatalf("heartbeats = %v", hbs)
	}
	if _, err := (DeadLetterReplayer{}).Replay(context.Background(), domain.DeadLetteredJob{}); err == nil {
		t.Fatal("replay without a bus succeeded")
	}
	if _, _, err := (AccountKeys{}).Bootstrap(context.Background(), nil); err == nil {
		t.Fatal("bootstrap without a key repo succeeded")
	}
}
