package main

import (
	"context"
	"log/slog"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/oauthserver"
	"github.com/opencairn-org/cairn-core/internal/usecase/providerlink"
)

// Leftovers of the oauth group still used outside it (sync_insight.go,
// admin_api.go, wire.go, tests); delete with their last caller.

type providerMeta = providerlink.Provider

// providerRegistry is the use case's map itself, so patching it (tests)
// reaches the connect flow.
var providerRegistry = providerlink.Providers

const oauthStateCookie = rest.OAuthConnectStateCookie

func oauthProvider(meta providerMeta) bool { return meta.OAuth() }

func providerActivityURL(provider, externalID string) string {
	return providerRegistry[provider].ActivityURL(externalID)
}

func appendQuery(rawURL string, params map[string]string) string {
	return oauthserver.AppendQuery(rawURL, params)
}

// gearSyncHook is the reconcile-time gear import publisher.
func gearSyncHook(app *App, logger *slog.Logger) func(ctx context.Context, a domain.ExternalAccount) error {
	return oauthProviderJobs(app, logger).ScheduledGearSync
}
