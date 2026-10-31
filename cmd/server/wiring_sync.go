package main

import (
	"context"
	"log/slog"
	"sort"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	syncuc "github.com/opencairn-org/cairn-core/internal/usecase/sync"
)

// Sync insight and rate-budget wiring for wire.go and the Connect server.

// syncInsight implements port.SyncInsight over the rate-limit KV, the pool
// registry + presence, the provider registry and the background discovery.
type syncInsight struct {
	app    *App
	logger *slog.Logger
}

var _ port.SyncInsight = syncInsight{}

func (s syncInsight) Budget(ctx context.Context, a domain.ExternalAccount) domain.RateBudget {
	if s.app.NATSBus == nil {
		return domain.RateBudget{}
	}
	return syncBudgets(s.app, s.app.NATSBus).For(ctx, a)
}

func (s syncInsight) Pools(ctx context.Context, provider string) []domain.WorkerPoolInfo {
	pools := knownPools(ctx, s.app, s.logger, provider)
	out := make([]domain.WorkerPoolInfo, 0, len(pools))
	for _, p := range pools {
		out = append(out, domain.WorkerPoolInfo{Name: p.name, Package: p.pkg, ParserVersion: p.parser, Release: p.release, Online: p.online, Compatible: !p.online || p.compatible})
	}
	return out
}

func (syncInsight) Providers() []domain.ProviderInfo {
	out := make([]domain.ProviderInfo, 0, len(providerRegistry))
	for id, meta := range providerRegistry {
		out = append(out, domain.ProviderInfo{ID: id, DisplayName: meta.DisplayName, OAuth: oauthProvider(meta)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (syncInsight) ActivityURL(provider, externalID string) string {
	return providerActivityURL(provider, externalID)
}

func (s syncInsight) StartFullSync(a domain.ExternalAccount, skipExisting bool) {
	syncImports(s.app, s.logger).StartFullSync(a, skipExisting)
}

// providerBudget is the live budget of a provider app; the bool reports an
// exhausted window (the reconcile budget gate in wire.go).
func providerBudget(ctx context.Context, bus syncuc.KVOpener, provider, appID, pool string) (domain.RateBudget, bool) {
	if bus == nil {
		return domain.RateBudget{}, false
	}
	return (&syncuc.Budgets{Bus: bus}).Of(ctx, provider, appID, pool)
}

// accountAppID is the hash of the account's connection app, "" when unknown.
func accountAppID(ctx context.Context, app *App, a domain.ExternalAccount) string {
	return syncBudgets(app, nil).AppID(ctx, a)
}
