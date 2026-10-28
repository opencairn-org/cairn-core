package main

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

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
	raw, _ := providerBudget(ctx, s.app.NATSBus, a.Provider, accountAppID(ctx, s.app, a), a.AssignedWorkerName)
	var out domain.RateBudget
	out.Short = budgetWindow(raw["short"])
	out.Daily = budgetWindow(raw["daily"])
	return out
}

// budgetWindow converts one providerBudget window map.
func budgetWindow(v any) *domain.BudgetWindow {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	w := &domain.BudgetWindow{}
	if n, ok := m["used"].(int); ok {
		w.Used = n
	}
	if n, ok := m["limit"].(int); ok {
		w.Limit = n
	}
	if s, ok := m["resets_at"].(string); ok {
		w.ResetsAt, _ = time.Parse(time.RFC3339, s)
	}
	return w
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
	go runFullDiscovery(s.app, s.logger, a, skipExisting)
	s.logger.Info("full discovery started", "account_id", a.ID, "skip_existing", skipExisting)
}
