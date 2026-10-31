package main

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	workerv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/worker/v1"
	natsadapter "github.com/opencairn-org/cairn-core/internal/adapter/secondary/nats"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
	"github.com/opencairn-org/cairn-core/internal/usecase/fleet"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
)

// Leftovers of the admin & ops group still referenced from files outside it
// (other handler groups, wire.go, connect_rpc.go, result_router.go). The
// integrator folds them into wire.go / the rest adapter once every group landed.

// actionErrorOf (httputil.go) lost its last caller here; delete both together.
var _ = actionErrorOf

// --- request auth for handlers still in this package ---

// resolveAdminUser resolves the session user and confirms they're an admin.
func resolveAdminUser(r *http.Request, app *App) (domain.User, bool) {
	return restAuth(app).Admin(r)
}

// resolveModeratorUser admits admins and moderators.
func resolveModeratorUser(r *http.Request, app *App) (domain.User, bool) {
	return restAuth(app).Moderator(r)
}

// restActor builds the audit actor of an admin request.
func restActor(r *http.Request, admin domain.User) audit.Actor {
	a := audit.UserActor(admin)
	a.IP = hostOnly(clientIP(r))
	a.UserAgent = truncateUA(r.UserAgent())
	return a
}

// hostOnly strips a port from an address for the audit log's inet column.
func hostOnly(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i > 0 && strings.Count(addr, ":") == 1 {
		addr = addr[:i]
	}
	return strings.Trim(addr, "[]")
}

// --- adapters and closures wired by wire.go / connect_rpc.go ---

// presenceStaleAfter is how long after a worker's last heartbeat its presence
// record is treated as a ghost: ~2 missed beats, under the 60s KV TTL.
const presenceStaleAfter = 45 * time.Second

// scanWorkerHeartbeats reads every live presence record, pruning stale ones.
func scanWorkerHeartbeats(ctx context.Context, app *App, logger *slog.Logger) []domain.WorkerHeartbeat {
	if app.NATSBus == nil {
		return []domain.WorkerHeartbeat{}
	}
	return app.NATSBus.WorkerHeartbeats(ctx, Version, presenceStaleAfter, logger)
}

func enrollmentProviders() []string {
	return slices.Sorted(maps.Keys(providerRegistry))
}

// natsKeyManager resolves the account-key adapter lazily: wire order sets the bus last.
type natsKeyManager struct{ app *App }

func (m natsKeyManager) keys() natsadapter.AccountKeys {
	k := natsadapter.AccountKeys{Secrets: m.app.SecretBox, Issuer: m.app.NATSCredentialIssuer}
	if m.app.SigningKeys != nil {
		k.Keys = m.app.SigningKeys
	}
	if m.app.Tx != nil {
		k.Tx = m.app.Tx
	}
	return k
}

func (m natsKeyManager) Bootstrap(ctx context.Context, by *domain.UserID) (domain.SigningKey, bool, error) {
	return m.keys().Bootstrap(ctx, by)
}

func (m natsKeyManager) Rotate(ctx context.Context, by *domain.UserID) (domain.SigningKey, string, error) {
	return m.keys().Rotate(ctx, by)
}

// natsReplayer republishes a dead-lettered payload over the current bus.
type natsReplayer struct{ app *App }

func (n natsReplayer) Replay(ctx context.Context, job domain.DeadLetteredJob) (string, error) {
	return natsadapter.DeadLetterReplayer{Bus: n.app.NATSBus}.Replay(ctx, job)
}

// geocodeBackfillRunner runs one batch in the background, logging the outcome.
func geocodeBackfillRunner(app *App, logger *slog.Logger) func(limit int) {
	if app.ComputeStartPlace == nil {
		return nil
	}
	return func(limit int) {
		goSafe(logger, "admin geocode backfill", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			n, err := app.ComputeStartPlace.BackfillBatch(ctx, limit)
			if err != nil {
				logger.Warn("admin geocode backfill failed", "error", err)
				return
			}
			logger.Info("admin geocode backfill complete", "resolved", n)
		})
	}
}

// liveWorkerCounts grades the fleet: compatible live instances are healthy,
// incompatible ones degraded, registered pools without a live instance offline.
func liveWorkerCounts(app *App, logger *slog.Logger) func(ctx context.Context) instance.WorkerCounts {
	return func(ctx context.Context) instance.WorkerCounts {
		var c instance.WorkerCounts
		online := map[string]bool{}
		for _, w := range currentWorkers(ctx, app, logger) {
			online[w.provider+"/"+w.name] = true
			if w.compatible {
				c.Healthy++
			} else {
				c.Degraded++
			}
		}
		c.Offline = offlinePools(ctx, app, online)
		return c
	}
}

// offlinePools counts registered, unretired pools without a live instance.
func offlinePools(ctx context.Context, app *App, online map[string]bool) int {
	if app.WorkerPools == nil {
		return 0
	}
	pools, err := app.WorkerPools.ListAll(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range pools {
		if p.RetiredAt == nil && !online[p.Provider+"/"+p.Name] {
			n++
		}
	}
	return n
}

// natsHealth reports the message bus connection for the health report.
func natsHealth(app *App) func() (bool, string) {
	return func() (bool, string) {
		if app.NATSBus == nil {
			return false, "message bus not configured"
		}
		return app.NATSBus.Health()
	}
}

// liveWorkerNames lists the pool names currently online.
func liveWorkerNames(app *App, logger *slog.Logger) func(ctx context.Context) []string {
	return func(ctx context.Context) []string {
		var out []string
		for _, w := range currentWorkers(ctx, app, logger) {
			out = append(out, w.name)
		}
		return out
	}
}

// --- pool routing (fleet.Routing) for the result router, pickers and sync ---

// presenceCache bounds presence-KV scans: routing, pickers and lists ask
// for live workers far more often than presence changes.
type presenceCache struct {
	mu   sync.Mutex
	at   time.Time
	list []liveWorker
}

const presenceCacheTTL = 5 * time.Second

func (c *presenceCache) get(now time.Time, scan func() []liveWorker) []liveWorker {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.at) < presenceCacheTTL {
		return c.list
	}
	c.list, c.at = scan(), now
	return c.list
}

// poolInfo is one known worker pool of a provider (docs/adr/0006).
type poolInfo struct {
	name, pkg, parser, release string
	online, compatible         bool
}

func poolRouting(app *App, logger *slog.Logger) *fleet.Routing {
	r := &fleet.Routing{Pools: app.WorkerPools, Presence: workerPresence{app: app, logger: logger}, Log: logger}
	if app.ExternalAccounts != nil {
		r.Accounts = app.ExternalAccounts
	}
	if app.Activities != nil {
		r.Activities = app.Activities
	}
	return r
}

// knownPools lists a provider's pools from the registry, with presence overlaid.
func knownPools(ctx context.Context, app *App, logger *slog.Logger, provider string) []poolInfo {
	pools := poolRouting(app, logger).Known(ctx, provider)
	out := make([]poolInfo, 0, len(pools))
	for _, p := range pools {
		out = append(out, poolInfo{name: p.Name, pkg: p.Package, parser: p.ParserVersion, release: p.Release, online: p.Online, compatible: p.Compatible})
	}
	return out
}

func knownPoolNames(ctx context.Context, app *App, logger *slog.Logger, provider string) []string {
	return poolRouting(app, logger).KnownNames(ctx, provider)
}

// checkResultPool verifies a result against its pool (fleet.Routing.CheckResultPool).
func checkResultPool(ctx context.Context, app *App, log *slog.Logger, subject string, jr *workerv1.JobResult) error {
	return poolRouting(app, log).CheckResultPool(ctx, subject, jr.GetWorkerName(), jr.GetWorkerPackage())
}

// checkResultAccount rejects an activity result from a pool that does not serve the account.
func checkResultAccount(ctx context.Context, app *App, job string, jr *workerv1.JobResult, ia *workerv1.ImportedActivity) error {
	ref := ia.GetRef()
	return poolRouting(app, slog.Default()).CheckResultAccount(ctx, job, jr.GetWorkerName(), jr.GetWorkerPackage(),
		fleet.ResultRef{Provider: ref.GetProvider(), ExternalAccountID: ref.GetExternalAccountId(), ExternalID: ref.GetExternalId()})
}
