package auth

import (
	"context"
	"testing"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

type guardAudit struct {
	port.AuditRepo
	entries []domain.AuditEntry
}

func (a *guardAudit) Record(_ context.Context, e domain.AuditEntry) error {
	a.entries = append(a.entries, e)
	return nil
}

type guardEvents struct{ got []domain.AdminEvent }

func (e *guardEvents) Publish(ev domain.AdminEvent) { e.got = append(e.got, ev) }

func TestLoginGuardLocksAndRecovers(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	rec, events := &guardAudit{}, &guardEvents{}
	g := &LoginGuard{Audit: &audit.Recorder{Repo: rec}, Events: events, Now: func() time.Time { return now },
		Window: time.Minute, Lock: 2 * time.Minute, MaxPerUser: 3, MaxPerAddress: 5}

	var nilGuard *LoginGuard
	if !nilGuard.Allow("a", "1.1.1.1") {
		t.Fatal("nil guard allows")
	}
	nilGuard.Failure(ctx, "a", "1.1.1.1")
	nilGuard.Success("a")

	for i := range 2 {
		g.Failure(ctx, "alice", "1.1.1.1")
		if !g.Allow("alice", "1.1.1.1") {
			t.Fatalf("locked after %d failures", i+1)
		}
	}
	g.Failure(ctx, "alice", "1.1.1.1")
	if g.Allow("alice", "1.1.1.1") || g.Allow("alice", "2.2.2.2") || !g.Allow("bob", "1.1.1.1") {
		t.Fatal("identifier lock must hit the identifier only")
	}
	if len(events.got) != 1 || events.got[0].Type != domain.AdminEventAuthFloodDetected || events.got[0].Params["key"] != "id:alice" || events.got[0].Params["failures"] != "3" {
		t.Fatalf("flood event: %+v", events.got)
	}
	// Every failure is audited at info; the lock adds a warn entry.
	var info, warn int
	for _, e := range rec.entries {
		switch e.Severity {
		case domain.AuditInfo:
			info++
		case domain.AuditWarn:
			warn++
		}
	}
	if info != 3 || warn != 1 {
		t.Fatalf("audit: %d info %d warn", info, warn)
	}
	// More failures while locked do not re-raise the event.
	g.Failure(ctx, "alice", "1.1.1.1")
	if len(events.got) != 1 {
		t.Fatalf("event per lock: %d", len(events.got))
	}
	// The lock expires with time; the window has moved on, so the tally restarts.
	now = now.Add(3 * time.Minute)
	if !g.Allow("alice", "1.1.1.1") {
		t.Fatal("lock expired")
	}
	g.Failure(ctx, "alice", "1.1.1.1")
	if !g.Allow("alice", "1.1.1.1") {
		t.Fatal("window restarted")
	}
	g.Success("alice")

	// Address lock across many identifiers; a success does not clear it.
	for i := range 5 {
		g.Failure(ctx, "user"+string(rune('a'+i)), "9.9.9.9")
	}
	if g.Allow("someone", "9.9.9.9") {
		t.Fatal("address locked")
	}
	g.Success("someone")
	if g.Allow("someone", "9.9.9.9") || len(events.got) != 2 || events.got[1].Params["key"] != "ip:9.9.9.9" {
		t.Fatalf("address lock persists: %+v", events.got)
	}
}
