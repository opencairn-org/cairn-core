package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/workeroauth"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/oauthserver"
)

// Provider registry reads and OAuth-connect wiring shared by the
// composition root. Provider meta comes from the workers' manifests
// (app.Providers); nothing is compiled in.

const oauthStateCookie = rest.OAuthConnectStateCookie

// providerCache serves provider meta to read paths (activity links, provider
// lists) without a DB round-trip per call; the manifest watcher invalidates.
type providerCache struct {
	mu   sync.Mutex
	at   time.Time
	list []domain.ProviderMeta
}

const providerCacheTTL = 30 * time.Second

// providerMetas lists the known providers, cached for providerCacheTTL.
func (app *App) providerMetas(ctx context.Context, logger *slog.Logger) []domain.ProviderMeta {
	if app.Providers == nil {
		return nil
	}
	c := &app.providers
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < providerCacheTTL {
		return c.list
	}
	list, err := app.Providers.List(ctx)
	if err != nil {
		logger.Warn("list providers failed", "error", err)
		return c.list
	}
	c.list, c.at = list, time.Now()
	return list
}

func (app *App) invalidateProviders() {
	app.providers.mu.Lock()
	app.providers.at = time.Time{}
	app.providers.mu.Unlock()
}

func providerActivityURL(app *App, provider, externalID string) string {
	for _, p := range app.providerMetas(context.Background(), slog.Default()) {
		if p.ID == provider {
			return p.ActivityURL(externalID)
		}
	}
	return ""
}

// appExchanger exchanges OAuth codes through app.CodeExchanger, or the
// provider's pool over the job bus; resolved per call so tests can swap it.
type appExchanger struct{ app *App }

func (e appExchanger) ExchangeCode(ctx context.Context, provider, pool string, conn domain.UserProviderConfig, in port.CodeExchange) (port.TokenState, string, error) {
	ex := e.app.CodeExchanger
	if ex == nil {
		if e.app.NATSBus == nil {
			return port.TokenState{}, "", workeroauth.ErrNoBus
		}
		ex = &workeroauth.Exchanger{Bus: e.app.NATSBus}
	}
	return ex.ExchangeCode(ctx, provider, pool, conn, in)
}

func appendQuery(rawURL string, params map[string]string) string {
	return oauthserver.AppendQuery(rawURL, params)
}

// reconcileHooks publishes the gear and health-metric imports that ride
// along with each reconcile; each only when the account's pool declares it.
func reconcileHooks(app *App, logger *slog.Logger) func(ctx context.Context, a domain.ExternalAccount) error {
	jobs := oauthProviderJobs(app, logger)
	return func(ctx context.Context, a domain.ExternalAccount) error {
		return errors.Join(jobs.ScheduledGearSync(ctx, a), jobs.ScheduledMetricsSync(ctx, a))
	}
}
