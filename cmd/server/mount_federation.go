package main

import (
	"context"
	"log/slog"
	"net/http"

	appub "github.com/opencairn-org/cairn-core/internal/adapter/primary/activitypub"
	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	apclient "github.com/opencairn-org/cairn-core/internal/adapter/secondary/activitypub"
	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/federation"
)

// fedClient is the one ActivityPub HTTP client; mountRestFederation may
// loosen it before anything is served.
var fedClient = apclient.NewClient()

// fedAudience is the shared visibility resolver, nil until wired.
func fedAudience(app *App) federation.Audience {
	if app.Visibility2 == nil {
		return nil
	}
	return app.Visibility2
}

func fedRemote(app *App) *federation.Remote {
	r := &federation.Remote{Client: fedClient}
	if app.FederationActors != nil {
		r.Actors = app.FederationActors
	}
	if app.FederationBlocks != nil {
		r.Blocks = app.FederationBlocks
	}
	return r
}

func fedSender(app *App) *federation.Sender {
	s := &federation.Sender{Client: fedClient}
	if app.FederationKeys != nil {
		s.Keys = app.FederationKeys
	}
	return s
}

// fedPublisher builds the publisher from the App as it stands now.
func fedPublisher(app *App, logger *slog.Logger) *federation.Publisher {
	p := &federation.Publisher{
		Enabled:  app.FederationEnabled,
		Instance: federation.NewInstance(app.PublicBaseURL),
		Audience: fedAudience(app),
		Remote:   fedRemote(app),
		Sender:   fedSender(app),
		Go:       syncGo(logger),
		Log:      logger,
	}
	if app.Users != nil {
		p.Users = app.Users
	}
	if app.FederationKeys != nil {
		p.Keys = app.FederationKeys
	}
	if app.FederationFollows != nil {
		p.Follows = app.FederationFollows
	}
	if app.FederationDeliveries != nil {
		p.Deliveries = app.FederationDeliveries
	}
	return p
}

// fedLazyPublisher resolves the App at publish time: wire.go builds it
// before the repos and the visibility resolver exist.
type fedLazyPublisher struct {
	app *App
	log *slog.Logger
}

func (p fedLazyPublisher) PublishCreate(ctx context.Context, act domain.Activity) {
	fedPublisher(p.app, p.log).PublishCreate(ctx, act)
}

func (p fedLazyPublisher) PublishDelete(ctx context.Context, userID domain.UserID, activityID domain.ActivityID) {
	fedPublisher(p.app, p.log).PublishDelete(ctx, userID, activityID)
}

// newFederationPublisher is the port.FederationPublisher of wire.go and connect_rpc.go.
func newFederationPublisher(app *App, logger *slog.Logger) port.FederationPublisher {
	return fedLazyPublisher{app: app, log: logger}
}

// mountRestFederation mounts the ActivityPub protocol routes and the
// session-gated federation API. The caller checks the instance flag.
func mountRestFederation(mux *http.ServeMux, app *App, cfg *config.Config, logger *slog.Logger) {
	// Trusted private networks (LAN, interop harness) host peers on private IPs over http.
	if cfg.Federation.AllowPrivateHosts {
		fedClient.AllowPrivateHosts()
		logger.Warn("federation: SSRF dial guard disabled + WebFinger over http (CAIRN_FEDERATION_ALLOW_PRIVATE_HOSTS) — use only on trusted networks")
	}
	in := federation.NewInstance(cfg.HTTP.PublicBaseURL)
	remote, sender := fedRemote(app), fedSender(app)
	actors := &federation.Actors{
		Instance: in, Users: app.Users, Keys: app.FederationKeys, Follows: app.Follows,
		Activities: app.Activities, FedFollows: app.FederationFollows,
		Audience: fedAudience(app), Remote: remote, Sender: sender, Log: logger,
	}
	inbox := &federation.Inbox{
		Instance: in, Follows: app.FederationFollows, Feed: app.FederationFeed, Engagement: app.Engagement,
		Activities: app.Activities, Audience: fedAudience(app), Sender: sender, Go: syncGo(logger), Log: logger,
	}
	if app.InboxDedup != nil {
		inbox.Dedup = app.InboxDedup
	}
	version, _, _ := buildInfo()
	(&appub.Handlers{
		Actors: actors, Inbox: inbox, Remote: remote,
		// ~120 inbound activities/min per remote domain; the bucket map is capped.
		InboxAllow: newIPRateLimiter(120, nil).allow,
		Version:    version,
		Log:        logger,
	}).Mount(mux)
	(&rest.FederationHandlers{Auth: restAuth(app), Actors: actors, Blocks: app.FedBlocks, Log: logger}).Mount(mux)
	logger.Info("federation (ActivityPub phases 1–3) endpoints mounted",
		"host", in.Host, "paths", []string{"/.well-known/webfinger", "/.well-known/nodeinfo", "/users/{username}"})
}

// runFederationDeliveryScheduler drains the durable outbound delivery queue until ctx ends.
func runFederationDeliveryScheduler(ctx context.Context, logger *slog.Logger, app *App) {
	(&federation.Deliveries{
		Repo: app.FederationDeliveries, Remote: fedRemote(app), Sender: fedSender(app),
		Log: logger.With("component", "federation_delivery"),
	}).Run(ctx)
}
