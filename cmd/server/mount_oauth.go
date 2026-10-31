package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/mcp"
	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/adapter/primary/workerrpc"
	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/provideroauth"
	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/oauthserver"
	"github.com/opencairn-org/cairn-core/internal/usecase/owndata"
	"github.com/opencairn-org/cairn-core/internal/usecase/providerlink"
)

// oauthDeviceRequestsPerMinute caps the device endpoints per client address.
const oauthDeviceRequestsPerMinute = 20

// oauthServerUseCase is the authorization server over app.OAuth.
func oauthServerUseCase(app *App, auth config.AuthConfig) *oauthserver.Server {
	return &oauthserver.Server{Repo: app.OAuth, Config: oauthserver.Config{
		AuthCodeTTL: auth.OAuthAuthCodeTTL, AccessTokenTTL: auth.OAuthAccessTokenTTL,
		RefreshTokenTTL: auth.OAuthRefreshTokenTTL, DeviceCodeTTL: auth.OAuthDeviceCodeTTL,
		DevicePollInterval: auth.OAuthDevicePollInterval,
	}}
}

// mountRestOAuthServer mounts the OAuth 2.1 authorization server; no-op
// unless app.OAuth is set.
func mountRestOAuthServer(mux *http.ServeMux, app *App, logger *slog.Logger, baseURL string, auth config.AuthConfig) {
	if app.OAuth == nil {
		return
	}
	limiter := newIPRateLimiter(oauthDeviceRequestsPerMinute, time.Now)
	h := &rest.OAuthServerHandlers{
		Auth: restAuth(app), Server: oauthServerUseCase(app, auth), Devices: app.DeviceAuthorizations, Log: logger,
		BaseURL: strings.TrimRight(baseURL, "/"), DynamicRegistration: auth.OAuthAllowDynamicReg,
		DeviceLimit: limiter.allow,
	}
	h.Mount(mux)
	logger.Info("oauth authorization server mounted",
		"dynamic_registration", auth.OAuthAllowDynamicReg, "issuer", h.BaseURL)
}

// mountRestMCP mounts the read-only MCP endpoint; it authenticates with the
// authorization server's access tokens, so it needs app.OAuth.
func mountRestMCP(mux *http.ServeMux, app *App, logger *slog.Logger, baseURL string, auth config.AuthConfig) {
	if app.OAuth == nil {
		return
	}
	(&mcp.Server{
		Tokens:  oauthServerUseCase(app, auth),
		Data:    &owndata.Reader{Activities: app.Activities, BestEfforts: app.BestEfforts, Users: app.Users},
		BaseURL: strings.TrimRight(baseURL, "/"),
	}).Mount(mux)
	logger.Info("mcp server mounted", "path", "/mcp", "tools", mcp.ToolCount())
}

// mountRestConnections mounts provider connections and the account-connect
// round-trip.
func mountRestConnections(mux *http.ServeMux, app *App, logger *slog.Logger, publicURL string) {
	(&rest.ConnectionHandlers{
		Auth: restAuth(app), Connections: oauthConnections(app, logger), Accounts: app.Accounts, Log: logger,
		PublicURL: publicURL, SecureCookie: cookieSecure,
	}).Mount(mux)
	logger.Info("oauth connect endpoints mounted", "providers", len(providerlink.Providers))
}

// mountRestAccountJobs mounts the gear-sync and health-import triggers.
func mountRestAccountJobs(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.ExternalAccounts == nil {
		return
	}
	(&rest.AccountJobHandlers{
		Auth: restAuth(app), Jobs: oauthProviderJobs(app, logger),
		HealthImport: app.UserProviderConfigs != nil,
	}).Mount(mux)
}

func oauthConnections(app *App, logger *slog.Logger) *providerlink.Connections {
	c := &providerlink.Connections{
		Providers: providerlink.Providers, Pools: oauthPools{app: app, logger: logger},
		Exchanger: &provideroauth.Exchanger{}, Bus: oauthJobBus(app), Jobs: oauthProviderJobs(app, logger), Log: logger,
	}
	// Typed nils would make the use case's nil checks lie.
	if app.UserProviderConfigs != nil {
		c.Configs = app.UserProviderConfigs
	}
	if app.ExternalAccounts != nil {
		c.Accounts = app.ExternalAccounts
	}
	if app.OAuthTokens != nil {
		c.Tokens = app.OAuthTokens
	}
	return c
}

func oauthProviderJobs(app *App, logger *slog.Logger) *providerlink.Jobs {
	j := &providerlink.Jobs{
		Bus: oauthJobBus(app), Router: presenceWorkerRouter{app: app, logger: logger},
		Pools: oauthPools{app: app, logger: logger}, Log: logger,
	}
	if app.ExternalAccounts != nil {
		j.Accounts = app.ExternalAccounts
	}
	return j
}

func oauthWorkerTokens(app *App, logger *slog.Logger) *providerlink.WorkerTokens {
	t := &providerlink.WorkerTokens{Bus: oauthJobBus(app), Log: logger}
	if app.OAuthTokens != nil {
		t.Tokens = app.OAuthTokens
	}
	if app.ExternalAccounts != nil {
		t.Accounts = app.ExternalAccounts
	}
	if app.UserProviderConfigs != nil {
		t.Configs = app.UserProviderConfigs
	}
	return t
}

// oauthJobBus is app.NATSBus as a port, nil when no bus is wired.
func oauthJobBus(app *App) port.JobBus {
	if app.NATSBus == nil {
		return nil
	}
	return app.NATSBus
}

// startOAuthTokenHandlers serves the worker token store on
// cairn.tokens.*.{get,store,needs_reauth}; nil when no bus is wired.
func startOAuthTokenHandlers(ctx context.Context, app *App, logger *slog.Logger) ([]port.Subscription, error) {
	if app.NATSBus == nil || app.OAuthTokens == nil {
		return nil, nil
	}
	log := logger.With("component", "oauth_token_handler")
	return (&workerrpc.Tokens{Tokens: oauthWorkerTokens(app, log), Log: log}).Start(ctx, app.NATSBus)
}

// startGarminCredsHandler serves cairn.creds.garmin.get; nil when not wired.
func startGarminCredsHandler(ctx context.Context, app *App, logger *slog.Logger) (port.Subscription, error) {
	if app.NATSBus == nil || app.ExternalAccounts == nil || app.UserProviderConfigs == nil {
		return nil, nil
	}
	return (&workerrpc.GarminLogins{Tokens: oauthWorkerTokens(app, logger), Log: logger}).Start(ctx, app.NATSBus)
}

// oauthPools implements port.PoolDirectory and port.PoolCapabilities over
// the pool registry and worker presence.
type oauthPools struct {
	app    *App
	logger *slog.Logger
}

func (p oauthPools) KnownPoolNames(ctx context.Context, provider string) []string {
	return knownPoolNames(ctx, p.app, p.logger, provider)
}

func (p oauthPools) PoolDeclares(ctx context.Context, provider, pool string, dt capability.DataType) bool {
	for _, w := range currentWorkers(ctx, p.app, p.logger) {
		if w.compatible && w.provider == provider && w.name == pool && w.capabilities.CanRead(dt) {
			return true
		}
	}
	return false
}
