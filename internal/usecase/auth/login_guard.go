package auth

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

// Login guard defaults: a sliding window of failures per identifier and per
// client address, and a temporary lock once a window overflows (OWASP
// "temporary account lockout" rather than a permanent one).
const (
	DefaultLoginWindow        = 10 * time.Minute
	DefaultLoginLock          = 10 * time.Minute
	DefaultMaxFailuresPerUser = 10
	DefaultMaxFailuresPerIP   = 20
)

// LoginGuard throttles password sign-in failures. Every failure lands in
// the audit log; crossing a threshold locks the key for a while and raises
// one auth-flood admin event per lock. A nil guard allows everything.
type LoginGuard struct {
	Audit  *audit.Recorder
	Events port.AdminEventSink
	Now    func() time.Time

	Window        time.Duration
	Lock          time.Duration
	MaxPerUser    int
	MaxPerAddress int

	mu     sync.Mutex
	fails  map[string][]time.Time
	locked map[string]time.Time
}

func (g *LoginGuard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now().UTC()
}

func (g *LoginGuard) settings() (window, lock time.Duration, perUser, perIP int) {
	window, lock, perUser, perIP = g.Window, g.Lock, g.MaxPerUser, g.MaxPerAddress
	if window <= 0 {
		window = DefaultLoginWindow
	}
	if lock <= 0 {
		lock = DefaultLoginLock
	}
	if perUser <= 0 {
		perUser = DefaultMaxFailuresPerUser
	}
	if perIP <= 0 {
		perIP = DefaultMaxFailuresPerIP
	}
	return window, lock, perUser, perIP
}

func (g *LoginGuard) init() {
	if g.fails == nil {
		g.fails = map[string][]time.Time{}
		g.locked = map[string]time.Time{}
	}
}

// Allow reports whether a sign-in attempt for the identifier from the
// address may proceed; a locked key refuses before the password is checked.
func (g *LoginGuard) Allow(identifier, address string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	now := g.now()
	for _, key := range []string{"id:" + identifier, "ip:" + address} {
		if until, ok := g.locked[key]; ok {
			if now.Before(until) {
				return false
			}
			delete(g.locked, key)
		}
	}
	return true
}

// Failure records a failed attempt and locks the keys that overflowed.
func (g *LoginGuard) Failure(ctx context.Context, identifier, address string) {
	if g == nil {
		return
	}
	window, lock, perUser, perIP := g.settings()
	g.mu.Lock()
	g.init()
	now := g.now()
	g.prune(now.Add(-window))
	overflowed := map[string]int{}
	for key, limit := range map[string]int{"id:" + identifier: perUser, "ip:" + address: perIP} {
		g.fails[key] = append(g.fails[key], now)
		if _, alreadyLocked := g.locked[key]; alreadyLocked {
			continue
		}
		if n := len(g.fails[key]); n >= limit {
			g.locked[key] = now.Add(lock)
			overflowed[key] = n
		}
	}
	g.mu.Unlock()

	actor := audit.Actor{Username: identifier, IP: address}
	g.Audit.Record(ctx, actor, domain.AuditLoginFailed, domain.AuditInfo, "login", identifier, nil, map[string]string{"ip": address})
	for key, n := range overflowed {
		g.Audit.Record(ctx, actor, domain.AuditLoginFailed, domain.AuditWarn, "login", key, nil,
			map[string]string{"ip": address, "failures": strconv.Itoa(n), "locked_for": lock.String()})
		if g.Events != nil {
			g.Events.Publish(domain.NewAdminEvent(domain.AdminEventAuthFloodDetected, domain.AuditWarn, "admin.auth_flood_detected").
				With("key", key).With("identifier", identifier).With("ip", address).With("failures", strconv.Itoa(n)).With("locked_for", lock.String()))
		}
	}
}

// Success clears the identifier's failures; the address keeps its tally.
func (g *LoginGuard) Success(identifier string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	delete(g.fails, "id:"+identifier)
	delete(g.locked, "id:"+identifier)
}

// prune drops failures older than the window and keys left empty.
func (g *LoginGuard) prune(before time.Time) {
	for key, ts := range g.fails {
		i := 0
		for i < len(ts) && ts[i].Before(before) {
			i++
		}
		if i == len(ts) {
			delete(g.fails, key)
			continue
		}
		g.fails[key] = ts[i:]
	}
}
