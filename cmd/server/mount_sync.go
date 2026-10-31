package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/port"
	syncuc "github.com/opencairn-org/cairn-core/internal/usecase/sync"
)

// syncGo runs background work under goSafe.
func syncGo(logger *slog.Logger) func(name string, fn func()) {
	return func(name string, fn func()) { goSafe(logger, name, fn) }
}

// syncImports builds the imports use case from the App as it stands.
func syncImports(app *App, logger *slog.Logger) *syncuc.Imports {
	imp := &syncuc.Imports{
		Accounts:   app.ExternalAccounts,
		Sources:    app.Activities,
		Queue:      app.ImportQueue,
		Bus:        app.NATSBus,
		Router:     presenceWorkerRouter{app: app, logger: logger},
		Pools:      app.WorkerPools,
		Insight:    syncInsight{app: app, logger: logger},
		Reimporter: newSourceReimporter(app, logger),
		Go:         syncGo(logger),
		Log:        logger,
	}
	if app.ImportEvents != nil {
		imp.Events = app.ImportEvents
	}
	return imp
}

// syncBudgets reads provider budgets over bus.
func syncBudgets(app *App, bus syncuc.KVOpener) *syncuc.Budgets {
	b := &syncuc.Budgets{Bus: bus}
	if app.UserProviderConfigs != nil {
		b.Connections = app.UserProviderConfigs
	}
	return b
}

// mountRestSync mounts the connection/sync routes; they need NATS.
func mountRestSync(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.NATSBus == nil {
		logger.Info("sync endpoints not mounted: NATS not wired")
		return
	}
	(&rest.SyncHandlers{
		Auth:               restAuth(app),
		Accounts:           app.Accounts,
		Imports:            syncImports(app, logger),
		DefaultPollSeconds: int(app.DefaultPollInterval.Seconds()),
		Log:                logger,
	}).Mount(mux)
	logger.Info("sync endpoints mounted")
}

// mountRestWebhooks mounts the provider webhook forwarder; without NATS it answers 503.
func mountRestWebhooks(mux *http.ServeMux, app *App, logger *slog.Logger) {
	h := &rest.WebhookHandlers{
		// 120/min per IP is far above any provider's webhook cadence.
		Allow: newIPRateLimiter(120, nil).allow,
	}
	if app.NATSBus != nil {
		h.Forwarder = &syncuc.WebhookForwarder{Bus: app.NATSBus, Log: logger.With("component", "webhook_forwarder")}
	}
	h.Mount(mux)
}

// runImportQueueProcessor drains the persisted import queue until ctx ends.
func runImportQueueProcessor(ctx context.Context, logger *slog.Logger, app *App) {
	syncImports(app, logger).RunQueueProcessor(ctx)
}

// startAccountLookupHandler answers workers resolving a webhook's provider
// owner id into an account, on the legacy subject and the per-pool one
// (docs/adr/0006).
func startAccountLookupHandler(ctx context.Context, app *App, logger *slog.Logger) (port.Subscription, error) {
	if app.NATSBus == nil || app.ExternalAccounts == nil {
		return nil, nil
	}
	lookup := &syncuc.AccountLookup{Accounts: app.ExternalAccounts, Log: logger.With("component", "account_lookup_handler")}
	sub, err := app.NATSBus.RespondTo(ctx, "cairn.accounts.lookup_by_provider_ext",
		func(ctx context.Context, body []byte) ([]byte, error) { return lookup.Respond(ctx, body), nil })
	if err != nil {
		return nil, err
	}
	poolSub, err := app.NATSBus.RespondToSubject(ctx, "cairn.accounts.lookup.*.*",
		func(ctx context.Context, subject string, body []byte) ([]byte, error) {
			return lookup.RespondPool(ctx, subject, body), nil
		})
	if err != nil {
		_ = sub.Close(ctx)
		return nil, err
	}
	lookup.Log.Info("account lookup handler active on cairn.accounts.lookup_by_provider_ext")
	return multiSubscription{sub, poolSub}, nil
}

// multiSubscription closes several subscriptions as one.
type multiSubscription []port.Subscription

func (m multiSubscription) Close(ctx context.Context) error {
	var err error
	for _, s := range m {
		err = errors.Join(err, s.Close(ctx))
	}
	return err
}
