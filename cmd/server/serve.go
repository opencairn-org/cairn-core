package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	webui "github.com/opencairn-org/cairn-core/internal/adapter/primary/web"
	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/db"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/admin"
	syncuc "github.com/opencairn-org/cairn-core/internal/usecase/sync"
)

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the Cairn server (HTTP + Connect-RPC + worker control plane)",
		Long: `Start the Cairn server.

On startup the server verifies that the database schema matches the version
embedded in this binary. The behaviour is:

  * schema current             → start normally
  * schema older + AUTO_MIGRATE → apply pending migrations, then start
  * schema older               → refuse to start; run "cairn migrate up"
  * schema newer               → refuse to start; this binary is too old

Set CAIRN_DATABASE_AUTO_MIGRATE=true for single-node deployments where
upgrading the binary should also apply migrations in one step. In
multi-replica deployments leave it off and run "cairn migrate up" as a
discrete step in your upgrade pipeline.`,
		RunE: runServe,
	}
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.Log)
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ------------------------------------------------------------------
	// Database
	// ------------------------------------------------------------------

	pool, err := serveOpenDatabase(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	// ------------------------------------------------------------------
	// Schema version check / auto-migrate
	// ------------------------------------------------------------------

	if err := ensureSchema(ctx, logger, pool, cfg.Database.AutoMigrate); err != nil {
		return err
	}

	// ------------------------------------------------------------------
	// Construct the application dependency graph
	// ------------------------------------------------------------------

	app, err := newApp(ctx, pool, cfg, logger)
	if err != nil {
		return fmt.Errorf("wire app: %w", err)
	}
	defer func() {
		if app.NATSBus != nil {
			app.NATSBus.Close()
		}
	}()

	if err := serveBootstrapAdmin(ctx, app, cfg, logger); err != nil {
		return err
	}

	// ------------------------------------------------------------------
	// HTTP server: health, REST, Connect-RPC, embedded UI
	// ------------------------------------------------------------------

	mux := http.NewServeMux()
	secureCookiesAlways = strings.HasPrefix(strings.ToLower(cfg.HTTP.PublicBaseURL), "https://")
	serveMountCore(ctx, mux, app, pool, cfg, logger)
	serveMountFeatures(mux, app, cfg, logger)
	serveMountPlatform(mux, app, logger)
	srv := serveNewHTTPServer(ctx, cfg, mux)

	// Subscriptions/watchers are closed in reverse start order on return.
	var cleanup serveCleanup
	defer cleanup.run()
	if err := serveStartBusHandlers(ctx, app, logger, &cleanup); err != nil {
		return err
	}
	if err := serveStartPipelines(ctx, app, logger, &cleanup); err != nil {
		return err
	}
	serveStartBackgroundLoops(ctx, app, cfg, logger)

	return serveListen(ctx, srv, cfg, logger)
}

// serveOpenDatabase opens the Postgres pool.
func serveOpenDatabase(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	logger.Info("opening database pool",
		"max_conns", cfg.Database.MaxConns,
		"min_conns", cfg.Database.MinConns,
	)

	pool, err := db.Open(ctx, db.Config{
		URL:                      cfg.Database.URL,
		MaxConns:                 cfg.Database.MaxConns,
		MinConns:                 cfg.Database.MinConns,
		StatementTimeout:         cfg.Database.StatementTimeout,
		LockTimeout:              cfg.Database.LockTimeout,
		IdleInTransactionTimeout: cfg.Database.IdleInTransactionTimeout,
		ApplicationName:          cfg.Database.ApplicationName,
		AutoMigrate:              cfg.Database.AutoMigrate,
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return pool, nil
}

// serveBootstrapAdmin creates the initial admin when configured (idempotent).
func serveBootstrapAdmin(ctx context.Context, app *App, cfg *config.Config, logger *slog.Logger) error {
	bootRes, err := app.BootstrapAdmin.Execute(ctx, admin.BootstrapAdminInput{
		Email:    cfg.Instance.BootstrapAdminEmail,
		Password: cfg.Instance.BootstrapAdminPassword,
	})
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	switch bootRes.Action {
	case admin.BootstrapAdminAlreadyPresent:
		logger.Info("bootstrap admin: instance already has admin(s)",
			"admin_count", bootRes.AdminCount)
	case admin.BootstrapAdminMissingInput:
		logger.Warn("bootstrap admin: no admin exists and CAIRN_INSTANCE_BOOTSTRAP_ADMIN_EMAIL/PASSWORD are unset; " +
			"the instance has no way to sign in")
	case admin.BootstrapAdminCreated:
		logger.Info("bootstrap admin: created initial admin user",
			"user_id", bootRes.CreatedUser.ID,
			"email", bootRes.CreatedUser.Email,
			"username", bootRes.CreatedUser.Username,
		)
	}
	return nil
}

// serveReadyzHandler reports whether every wired dependency is reachable.
func serveReadyzHandler(pool *pgxpool.Pool, app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Ready means: every wired dependency is reachable. Orchestrators route
		// traffic on this, so a half-up instance (DB ok but NATS/S3 down) must
		// report NOT ready. Optional deps (NATS/S3) are only checked when wired.
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		checks, ok := serveReadyzChecks(ctx, pool, app)

		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]any{"ready": ok, "checks": checks})
	}
}

// serveReadyzChecks probes postgres plus NATS/S3 when wired.
func serveReadyzChecks(ctx context.Context, pool *pgxpool.Pool, app *App) (map[string]string, bool) {
	checks := map[string]string{}
	ok := true
	fail := func(name string, err error) {
		checks[name] = "error: " + err.Error()
		ok = false
	}

	if err := pool.Ping(ctx); err != nil {
		fail("postgres", err)
	} else {
		checks["postgres"] = "ok"
	}
	if app.NATSBus != nil {
		if app.NATSBus.Conn().IsConnected() {
			checks["nats"] = "ok"
		} else {
			fail("nats", errors.New("not connected"))
		}
	}
	if app.BlobStore != nil {
		if err := app.BlobStore.Healthy(ctx); err != nil {
			fail("blob_store", err)
		} else {
			checks["blob_store"] = "ok"
		}
	}
	return checks, ok
}

// serveMountCore mounts health, admin ops, webhooks, sign-in and federation.
func serveMountCore(ctx context.Context, mux *http.ServeMux, app *App, pool *pgxpool.Pool, cfg *config.Config, logger *slog.Logger) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", serveReadyzHandler(pool, app))
	// Signed binary-content delivery; sets app.Content, which the REST and
	// Connect handlers below use to hand out photo/map/export URLs.
	mountContent(mux, app, cfg, logger)
	// Operator-only maintenance endpoints, session+admin-gated under /api/admin/*
	// (NATS account-key bootstrap/rotate, dead-letter inspect/replay, geocode
	// backfill). The legacy token-gated /admin/* smoketest surface was removed —
	// listings/recompute/detach now live on the Connect-RPC AdminService + the
	// per-activity /api/* surface.
	mountAdminOps(mux, app, logger)

	// Provider-agnostic webhook forwarder. POST /webhooks/{provider}
	// forwards raw bodies to cairn.webhooks.<provider>.event;
	// GET request/replies to cairn.webhooks.<provider>.verify.
	// All envelope decoding + signature validation + handshake protocol
	// lives in the provider's worker (see workersdk.WebhookEvent /
	// WebhookVerify). Core knows nothing about Strava, Garmin, etc.
	mountWebhookForwarder(mux, app, logger)
	logger.Info("generic webhook forwarder mounted", "path", "/webhooks/{provider}")

	// (DLQ /admin endpoints are mounted with the dev /admin/* surface above.)

	// OIDC sign-in endpoints. Providers come from CAIRN_OIDC_* env vars
	// (parsed in newApp); gated on app.OIDCLogin being non-nil.
	mountOIDCEndpoints(mux, app, logger)

	// ActivityPub federation (phase 1: read-only actor + discovery). Off unless
	// CAIRN_FEDERATION_ENABLED; per-user opt-in gates each actor.
	if cfg.Federation.Enabled {
		mountFederation(mux, app, cfg, logger)
		// Drain the durable outbound delivery queue (Phase 3 push fan-out).
		if app.FederationDeliveries != nil {
			goSupervised(ctx, logger, "runFederationDeliveryScheduler", func(ctx context.Context) { runFederationDeliveryScheduler(ctx, logger, app) })
		}
	} else {
		logger.Info("federation disabled (CAIRN_FEDERATION_ENABLED=false)")
	}

	// Username/email + password sign-in. The /login page posts here and
	// GetLoginMethods advertises password as available, so this is the
	// default sign-in path for instances without an external IdP.
	mountPasswordAuth(mux, app, logger, cfg.Auth.SessionTTL)

	// WebAuthn / passkey ceremonies + management (REST).
	mountWebAuthn(mux, app, logger, cfg.HTTP.PublicBaseURL, cfg.Auth.SessionTTL)

	// Invite create (admin) + redeem (public self-service signup).
	mountInvites(mux, app, logger, cfg.HTTP.PublicBaseURL, cfg.Auth.SessionTTL)
	mountAuthRecovery(mux, app, logger, cfg.HTTP.PublicBaseURL, cfg.Auth.SessionTTL)

	// Personal access tokens (CLI/API auth).
	mountPATs(mux, app, logger)
	// Password change, sessions, linked identities, profile (AuthService twins).
	mountAccount(mux, app, logger)
	mountAuthorizedApps(mux, app)

	// OAuth 2.1 authorization server (native apps, third-party clients, MCP).
	if cfg.Auth.OAuthEnabled {
		mountOAuthServer(mux, app, logger, cfg.HTTP.PublicBaseURL, cfg.Auth)
		// MCP server (read-only, OAuth-protected) — depends on the AS.
		mountMCP(mux, app, logger, cfg.HTTP.PublicBaseURL)
	}
}

// serveMountFeatures mounts the user-facing REST feature endpoints.
func serveMountFeatures(mux *http.ServeMux, app *App, cfg *config.Config, logger *slog.Logger) {
	// Activity writes: upload, manual entry, source-from, source actions and
	// previews, manage, merge overrides/policies, athlete profile, metric store.
	mountAct(mux, app, logger)
	// Activity attachments (photos) — /api/activities/{id}/attachments.
	mountAttachmentEndpoints(mux, app, logger)

	// Per-user provider config + OAuth account-connect. Credentials are
	// per-user (each user brings their own OAuth app via the Connections UI);
	// there is no instance-global OAuth credential.
	mountOAuthConnect(mux, app, logger, cfg.HTTP.PublicBaseURL)

	// Landing dashboard: totals + by-sport + latest activities.
	mountOverview(mux, app, logger)

	// Analysis (training load) + Stats (insights) dashboards.
	mountAnalysis(mux, app, logger)
	mountStats(mux, app, logger)

	// Faceted/filterable/sortable activity feed (JSON REST for the SPA).
	mountActivitiesFeed(mux, app, logger)

	// An activity's segment efforts (joined to their segment).
	mountSegmentEfforts(mux, app, logger)
	mountSegmentDetail(mux, app, logger)
	mountSegmentAuthoring(mux, app, logger)
	mountZones(mux, app, logger)
	mountLaps(mux, app, logger)
	mountGarminConnect(mux, app)
	mountGearSync(mux, app, logger)
	mountMergedStream(mux, app)
	mountRecords(mux, app, logger)
	mountCustomRecords(mux, app, logger)
	mountHealth(mux, app, logger)
	mountReviewQueue(mux, app, logger)
	mountActivityExport(mux, app, logger)
	mountActivityMapImage(mux, app, cfg.Map, logger)
	mountActivitiesHeatmap(mux, app, logger)
	mountInstanceFeatures(mux, cfg, logger)
	mountActivitySimilar(mux, app, logger)
	mountBestEffortHistory(mux, app, logger)
	mountSegmentsList(mux, app, logger)
	serveMountSocial(mux, app, logger)
}

// serveMountSocial mounts social, moderation, club and notification endpoints.
func serveMountSocial(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mountSocial(mux, app, logger)
	mountHomeFeed(mux, app, logger)
	mountProfiles(mux, app, logger)
	mountRestShare(mux, app, logger)
	mountEngagement(mux, app, logger)
	mountModeration(mux, app, logger)
	mountQuota(mux, app, logger)
	mountClubs(mux, app, logger)
	mountNotificationEmailTest(mux, app, logger)
	mountNotificationPrefs(mux, app, logger)
	mountNotificationWebhooks(mux, app, logger)
	mountNotificationQuietHours(mux, app, logger)
	mountNotificationDeliveries(mux, app, logger)
}

// serveMountPlatform mounts admin API, sync, version, metrics, Connect-RPC and the web UI.
func serveMountPlatform(mux *http.ServeMux, app *App, logger *slog.Logger) {
	// Session+admin-gated admin API (worker onboarding) for the admin UI.
	mountAdminAPI(mux, app, logger)
	mountAdminUsers(mux, app, logger)
	mountAdminInstance(mux, app, logger)

	// Full-sync + import-queue endpoints (gated on NATS internally).
	mountSyncEndpoints(mux, app, logger)
	mountAccountLifecycle(mux, app, logger)

	// Build/version info (public) — shown in the UI footer.
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) {
		v, c, t := buildInfo()
		writeJSON(w, http.StatusOK, map[string]any{"version": v, "commit": c, "build_time": t})
	})

	// Prometheus metrics (unauthenticated scrape endpoint).
	{
		v, c, _ := buildInfo()
		setBuildInfoMetric(v, c)
	}
	mux.Handle("GET /metrics", metricsHandler())

	// Connect-RPC handlers under /cairn.v1.*/ — the user-facing surface
	// the SvelteKit frontend and (eventually) mobile/third-party clients
	// consume. Each service grows method-by-method off the Unimplemented
	// base so unfinished RPCs return CodeUnimplemented automatically.
	mountConnectRPC(mux, app, logger)

	// Embedded web UI at / (catch-all; loses to every specific pattern
	// above). Present only in production builds compiled with
	// `-tags embedweb` (docker/Dockerfile.core) — one binary serves API +
	// UI. In dev the binary embeds nothing and the SvelteKit Node container
	// behind Caddy serves the UI instead.
	if h, ok := webui.Handler(); ok {
		mux.Handle("/", h)
		logger.Info("embedded web UI mounted at /")
	} else {
		logger.Info("no embedded web UI in this build (dev: served by the web container)")
	}
}

// serveNewHTTPServer wraps mux in the middleware chain and builds the server.
func serveNewHTTPServer(ctx context.Context, cfg *config.Config, mux *http.ServeMux) *http.Server {
	// Per-caller request-rate ceiling (per session, else per IP). Generous by
	// default so the polling SPA isn't throttled; override via env.
	perMin := 600.0
	if v := os.Getenv("CAIRN_HTTP_RATE_LIMIT_PER_MIN"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			perMin = n
		}
	}
	httpLimiter := newIPRateLimiter(perMin, nil)

	// HTTP/1.1 plus cleartext HTTP/2 (h2c, prior knowledge): native Connect
	// and gRPC clients multiplex and stream over one connection without a
	// TLS-terminating proxy in front. Browsers never negotiate h2c, so the
	// SPA path is unchanged. TLS still comes from the reverse proxy.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	return &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           metricsMiddleware(securityHeaders(rateLimitMiddleware(httpLimiter, mux))),
		Protocols:         protocols,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
}

// serveClosable is anything runServe closes on shutdown (subscriptions, KV watchers).
type serveClosable interface {
	Close(ctx context.Context) error
}

// serveCleanup is a LIFO stack of shutdown closers, mirroring stacked defers.
type serveCleanup struct {
	fns []func()
}

// add registers closer to be closed on run, logging warnMsg on a close error.
func (c *serveCleanup) add(closer serveClosable, logger *slog.Logger, warnMsg string) {
	c.fns = append(c.fns, func() {
		if err := closer.Close(context.Background()); err != nil {
			logger.Warn(warnMsg, "error", err)
		}
	})
}

// run closes everything in reverse registration order.
func (c *serveCleanup) run() {
	for i := len(c.fns) - 1; i >= 0; i-- {
		c.fns[i]()
	}
}

// serveStartBusHandlers starts the auth-callout subscriber and the worker
// request/reply responders (OAuth tokens, Garmin creds, reauth, account lookup).
func serveStartBusHandlers(ctx context.Context, app *App, logger *slog.Logger, cleanup *serveCleanup) error {
	// ------------------------------------------------------------------
	// NATS auth-callout subscriber
	//
	// When NATS is wired, subscribe to $SYS.REQ.USER.AUTH so worker
	// connection attempts go through ProcessAuthCallout for JWT minting.
	// The subscription's lifecycle is the server's lifecycle — Close()
	// in the deferred cleanup at the bottom of runServe.
	// ------------------------------------------------------------------
	if app.NATSAuthCallout != nil {
		sub, err := app.NATSAuthCallout.Start(ctx)
		if err != nil {
			return fmt.Errorf("start auth-callout subscriber: %w", err)
		}
		cleanup.add(sub, logger, "auth-callout: close error")
	} else {
		logger.Info("auth-callout subscriber not started: NATS or CAIRN_NATS_AUTH_CALLOUT off")
	}

	// ------------------------------------------------------------------
	// OAuth token handlers
	//
	// Three request/reply subjects (get / store / needs_reauth) that
	// workers use for OAuth token state. Provider-agnostic — the same
	// handlers serve Strava, Garmin, Polar, etc.
	// ------------------------------------------------------------------
	oauthSubs, err := startOAuthTokenHandlers(ctx, app, logger)
	if err != nil {
		return fmt.Errorf("start oauth token handlers: %w", err)
	}
	for _, sub := range oauthSubs {
		cleanup.add(sub, logger, "oauth handler: close error")
	}
	// Garmin per-user credential handler (cairn.creds.garmin.get).
	if err := serveStartOptionalSub(ctx, app, logger, cleanup, startGarminCredsHandler,
		"start garmin creds handler: %w", "garmin creds handler: close error"); err != nil {
		return err
	}
	// Reauth notifier: turns needs_reauth events into user notifications.
	if err := serveStartOptionalSub(ctx, app, logger, cleanup, startReauthNotifier,
		"start reauth notifier: %w", "reauth notifier: close error"); err != nil {
		return err
	}
	// Account-lookup responder (workers resolve webhook owner_id → account).
	if err := serveStartOptionalSub(ctx, app, logger, cleanup, startAccountLookupHandler,
		"start account lookup handler: %w", "account lookup handler: close error"); err != nil {
		return err
	}
	if len(oauthSubs) == 0 {
		logger.Info("oauth token handlers not started: NATS adapter not wired")
	}
	return nil
}

// serveStartOptionalSub starts a responder that returns nil when not wired.
func serveStartOptionalSub(
	ctx context.Context,
	app *App,
	logger *slog.Logger,
	cleanup *serveCleanup,
	start func(context.Context, *App, *slog.Logger) (port.Subscription, error),
	startErrFmt, closeWarn string,
) error {
	sub, err := start(ctx, app, logger)
	if err != nil {
		return fmt.Errorf(startErrFmt, err)
	}
	if sub != nil {
		cleanup.add(sub, logger, closeWarn)
	}
	return nil
}

// serveStartPipelines starts blob presign handlers, the result router, the
// manifest watcher and the DLQ listener.
func serveStartPipelines(ctx context.Context, app *App, logger *slog.Logger, cleanup *serveCleanup) error {
	// ------------------------------------------------------------------
	// Blob presign handlers
	//
	// Two request/reply subjects (cairn.blobs.presign_upload.> and
	// cairn.blobs.presign_download.>) workers call to obtain short-
	// lived signed URLs. Gated on both NATSBus and BlobStore being
	// configured — without S3 the server simply doesn't subscribe.
	// ------------------------------------------------------------------
	blobSubs, err := startBlobHandlers(ctx, app, logger)
	if err != nil {
		return fmt.Errorf("start blob handlers: %w", err)
	}
	for _, sub := range blobSubs {
		cleanup.add(sub, logger, "blob handler: close error")
	}

	// ------------------------------------------------------------------
	// Result-ingest router
	//
	// Consumes cairn.results.fetch_source.> and runs each result through
	// the full ingest + follow-ups pipeline (best-efforts, segments,
	// training-load, PR-dispatch). Same lifecycle as the auth-callout
	// subscription — disabled silently when NATS isn't wired.
	// ------------------------------------------------------------------
	resultSub, err := startResultRouter(ctx, app, logger)
	if err != nil {
		return fmt.Errorf("start result router: %w", err)
	}
	if resultSub != nil {
		cleanup.add(resultSub, logger, "result router: close error")
	} else {
		logger.Info("result router not started: NATS adapter not wired")
	}

	// ------------------------------------------------------------------
	// Manifest watcher
	//
	// Watches the cairn_worker_manifests KV bucket. When a worker from a
	// given package+provider publishes a HIGHER version, all activity_sources
	// from that same package+provider with a lower version get flipped to
	// reimport_status='update_available'. The routing name/alias is irrelevant;
	// (provider, package, version) is the maintainer-upheld compatibility
	// contract. Closes the drift loop between worker upgrades and stale payloads.
	// ------------------------------------------------------------------
	manifestWatch, err := startManifestWatcher(ctx, app, logger)
	if err != nil {
		return fmt.Errorf("start manifest watcher: %w", err)
	}
	if manifestWatch != nil {
		cleanup.add(manifestWatch, logger, "manifest watcher: close error")
	} else {
		logger.Info("manifest watcher not started: NATS adapter not wired")
	}

	// ------------------------------------------------------------------
	// DLQ listener + admin endpoints
	//
	// Subscribes to JetStream's MaxDeliveries advisory and captures
	// dead-lettered jobs into Postgres. The /admin/dlq endpoints
	// expose the captured rows for operator inspection + replay.
	// ------------------------------------------------------------------
	dlqSub, err := startDLQListener(ctx, app, logger)
	if err != nil {
		return fmt.Errorf("start dlq listener: %w", err)
	}
	if dlqSub != nil {
		cleanup.add(dlqSub, logger, "dlq listener: close error")
	} else {
		logger.Info("dlq listener not started: NATS adapter or repo not wired")
	}
	return nil
}

// serveStartBackgroundLoops launches the queue processor, watchers, backfillers and schedulers.
func serveStartBackgroundLoops(ctx context.Context, app *App, cfg *config.Config, logger *slog.Logger) {
	// ------------------------------------------------------------------
	// Reconciliation scheduler
	//
	// Drives the periodic catch-up loop that finds external_accounts
	// due for reconciliation and publishes reconcile-jobs to NATS.
	// Acts as both drift-safety for webhook-subscribed accounts and
	// the primary import path for providers without webhooks.
	//
	// Disabled silently when the JobBus / Reconcile use case isn't
	// wired (NATS adapter not yet implemented). Operators can still
	// trigger reconciliation manually via the admin endpoint.
	// ------------------------------------------------------------------
	// Import-queue processor: drains the persisted queue paced + rate-limit-
	// aware. Gated on NATS (it dispatches fetch jobs to workers).
	if app.NATSBus != nil {
		goSupervised(ctx, logger, "runImportQueueProcessor", func(ctx context.Context) { runImportQueueProcessor(ctx, logger, app) })
	}

	// Worker-presence watcher: notifies admins when a previously-seen worker
	// stops heartbeating (its presence KV key expires). Gated on NATS + the
	// notifier.
	if app.NATSBus != nil && app.NotifyWorkerOffline != nil {
		goSupervised(ctx, logger, "runWorkerPresenceWatcher", func(ctx context.Context) { runWorkerPresenceWatcher(ctx, logger, app) })
	}

	// Admin-triggered maintenance jobs run one at a time for the server's lifetime.
	if app.Maintenance != nil {
		app.Maintenance.Start(ctx)
	}

	// Health watcher: raises an admin event when the instance leaves "healthy".
	if app.SystemHealth != nil && app.AdminEvents != nil {
		goSupervised(ctx, logger, "runHealthWatcher", func(ctx context.Context) { runHealthWatcher(ctx, logger, app) })
	}

	// Start-location backfiller: reverse-geocodes activities missing a
	// start_place. Self-paced by the geocoder's policy limiter (1 req/s for
	// OSM). Gated on the geocoder being wired (CAIRN_GEOCODER_ENABLED).
	if app.ComputeStartPlace != nil {
		goSupervised(ctx, logger, "runStartPlaceBackfiller", func(ctx context.Context) { runStartPlaceBackfiller(ctx, logger, app, cfg.Geocoder) })
	}

	// Notification-delivery retry processor: re-sends email/webhook deliveries
	// that failed transiently (failed_retryable rows whose next_retry_at has
	// elapsed), with capped exponential backoff. RetryDue is itself gated on
	// the deliveries + notifications repos being wired.
	if app.DeliverNotifications != nil {
		goSupervised(ctx, logger, "runNotificationRetryScheduler", func(ctx context.Context) { runNotificationRetryScheduler(ctx, logger, app) })
	}

	if cfg.Scheduler.Enabled && app.ReconcileAccount != nil {
		goSupervised(ctx, logger, "runReconcileScheduler", func(ctx context.Context) { runReconcileScheduler(ctx, logger, app, cfg.Scheduler) })
	} else {
		switch {
		case !cfg.Scheduler.Enabled:
			logger.Info("reconcile scheduler disabled by config (CAIRN_SCHEDULER_ENABLED=false)")
		case app.ReconcileAccount == nil:
			logger.Info("reconcile scheduler not started: JobBus not wired (NATS adapter pending)")
		}
	}
}

// serveListen runs the HTTP server until it fails or ctx is cancelled, then drains it.
func serveListen(ctx context.Context, srv *http.Server, cfg *config.Config, logger *slog.Logger) error {
	// Run the listener in a goroutine so we can react to ctx cancellation.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTP.Addr)
		var serveErr error
		if cfg.HTTP.TLSCertFile != "" {
			serveErr = srv.ListenAndServeTLS(cfg.HTTP.TLSCertFile, cfg.HTTP.TLSKeyFile)
		} else {
			serveErr = srv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	// Block until signal or listener exits.
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining HTTP server",
			"timeout", cfg.HTTP.ShutdownTimeout)
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
		defer cancelShutdown()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http shutdown returned error", "error", err)
		}
		return nil
	}
}

// ensureSchema enforces the version invariant on every server start.
//
// Decision matrix:
//
//	state                    AutoMigrate=false   AutoMigrate=true
//	current                  proceed             proceed
//	too old                  refuse              migrate, proceed
//	too new                  refuse              refuse (binary is older than schema)
//	internal error           refuse              refuse
func ensureSchema(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, autoMigrate bool) error {
	state, err := db.EnsureSchemaCurrent(ctx, pool)
	switch {
	case err == nil:
		logger.Info("schema current",
			"version", state.CurrentVersion,
			"embedded_latest", state.LatestVersion,
		)
		return nil

	case errors.Is(err, db.ErrSchemaTooOld):
		if !autoMigrate {
			return fmt.Errorf("database schema is at version %d but this binary expects %d (%d pending migration(s)) — run `cairn migrate up` or set CAIRN_DATABASE_AUTO_MIGRATE=true",
				state.CurrentVersion, state.LatestVersion, state.PendingCount)
		}
		logger.Warn("database schema is older than required — auto-migrating",
			"from", state.CurrentVersion,
			"to", state.LatestVersion,
			"pending", state.PendingCount,
		)
		t0 := time.Now()
		results, mErr := db.MigrateUp(ctx, pool)
		if mErr != nil {
			return fmt.Errorf("auto-migrate failed at startup: %w", mErr)
		}
		for _, r := range results {
			logger.Info("migration applied",
				"version", r.Source.Version,
				"name", r.Source.Path,
				"duration", r.Duration,
			)
		}
		logger.Info("auto-migrate complete",
			"applied", len(results),
			"duration", time.Since(t0),
		)
		return nil

	case errors.Is(err, db.ErrSchemaTooNew):
		return fmt.Errorf("database schema is at version %d but this binary only supports up to %d — upgrade the binary",
			state.CurrentVersion, state.LatestVersion)

	default:
		return fmt.Errorf("schema version check failed: %w", err)
	}
}

func newLogger(cfg config.LogConfig) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Format == "text" {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

// runStartPlaceBackfiller periodically reverse-geocodes activities that have a
// GPS stream but no start_place yet, populating the feed/detail subtitle.
//
// It is paced by the geocoder's own usage-policy limiter (1 req/s for OSM
// Nominatim), so a batch of N takes ~N seconds. When a tick resolves a full
// batch there is probably more work, so the next tick fires promptly; when a
// tick comes up empty the loop idles for TickInterval. New activities are
// picked up on the next idle tick — geocoding is intentionally NOT on the
// ingest hot path, both to keep ingest fast and to honour the rate limit.
func runStartPlaceBackfiller(
	ctx context.Context,
	logger *slog.Logger,
	app *App,
	cfg config.GeocoderConfig,
) {
	logger = logger.With("component", "start_place_backfiller")
	logger.Info("start-place backfiller started",
		"batch_size", cfg.BatchSize, "tick_interval", cfg.TickInterval)

	tick := cfg.TickInterval
	if tick <= 0 {
		tick = 2 * time.Minute
	}
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = 50
	}

	for {
		resolved, err := app.ComputeStartPlace.BackfillBatch(ctx, batch)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("start-place backfill tick failed", "error", err)
		} else if resolved > 0 {
			logger.Info("start-place backfill tick", "resolved", resolved)
		}

		// If a full batch was resolved there is likely more — loop again
		// promptly (the geocoder limiter already spaced the requests). Only
		// idle for the full interval when there was little or nothing to do.
		wait := tick
		if resolved >= batch {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// runReconcileScheduler ticks every cfg.TickInterval and asks
// ReconcileExternalAccount to handle all accounts currently due.
//
// Design notes:
//
//   - One scheduler per server process. In a multi-replica deployment,
//     every replica runs its own scheduler — that's safe because the
//     reconcile use case dedups jobs via Nats-Msg-Id bucketed to the
//     minute, so two tick-aligned replicas collapse to a single job.
//
//   - Errors are logged but never returned. The scheduler must keep
//     running across transient DB/NATS hiccups; the next tick retries.
//
//   - Graceful shutdown via ctx.Done. Any in-flight Execute call
//     completes (or aborts on its own ctx); no new tick fires after
//     the signal arrives.
//
//   - The first tick fires immediately on startup, not after one
//     interval — useful so a deploy doesn't pause reconciliation for
//     up to TickInterval seconds.
func runReconcileScheduler(
	ctx context.Context,
	logger *slog.Logger,
	app *App,
	cfg config.SchedulerConfig,
) {
	logger = logger.With("component", "reconcile_scheduler")
	logger.Info("reconcile scheduler started",
		"tick_interval", cfg.TickInterval,
		"polling_interval", cfg.PollingInterval,
		"webhook_drift", cfg.WebhookDriftInterval,
		"batch_size", cfg.BatchSize,
	)

	ticker := time.NewTicker(cfg.TickInterval)
	defer ticker.Stop()

	// First tick fires immediately so deploy-to-first-reconcile latency
	// equals query time, not TickInterval.
	tickFn := func() {
		tickCtx, cancel := context.WithTimeout(ctx, cfg.TickInterval)
		defer cancel()

		res, err := app.ReconcileAccount.Execute(tickCtx, syncuc.ReconcileInput{
			All:       true,
			BatchSize: cfg.BatchSize,
			Reason:    "scheduled",
		})
		if err != nil {
			logger.Warn("reconcile tick: top-level failure", "error", err)
			return
		}
		// Per-account errors come back in res.Errors; log them but keep
		// going — they're not fatal to the scheduler.
		if len(res.Errors) > 0 {
			for _, e := range res.Errors {
				logger.Warn("reconcile tick: per-account failure",
					"account_id", e.AccountID, "error", e.Err)
			}
		}
		if res.AccountsScheduled > 0 || res.AccountsSkipped > 0 {
			logger.Info("reconcile tick complete",
				"scheduled", res.AccountsScheduled,
				"skipped", res.AccountsSkipped,
				"errors", len(res.Errors),
			)
		}
	}

	tickFn() // immediate first tick
	for {
		select {
		case <-ctx.Done():
			logger.Info("reconcile scheduler shutting down")
			return
		case <-ticker.C:
			tickFn()
		}
	}
}

// runNotificationRetryScheduler ticks every minute and re-attempts due
// side-channel notification deliveries. The backoff schedule lives in the
// domain (NextDeliveryRetryAt); this just paces the polling. Stops with ctx.
func runNotificationRetryScheduler(ctx context.Context, logger *slog.Logger, app *App) {
	log := logger.With("component", "notification_retry")
	const tick = time.Minute
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	log.Info("notification retry scheduler started", "interval", tick.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			app.DeliverNotifications.RetryDue(ctx)
		}
	}
}

// runWorkerPresenceWatcher polls the worker-presence KV and notifies admins
// when a worker that was previously online stops heartbeating (its presence
// key expires past the TTL). It tracks the set of live worker keys between
// ticks; a key that was present last tick and is gone/stale this tick is an
// offline transition. On server restart the tracked set starts empty, so the
// first tick only populates it — no false "offline" for workers that were
// already up.
func runWorkerPresenceWatcher(ctx context.Context, logger *slog.Logger, app *App) {
	const tick = 30 * time.Second
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	w := &workerPresenceWatcher{
		app:            app,
		log:            logger.With("component", "worker_presence_watcher"),
		seen:           map[string]string{},
		manifestAbsent: map[string]int{},
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// workerPresenceManifestReapAfterTicks: worker name → consecutive ticks with no
// live presence. A manifest is reaped once its worker has been absent this many
// ticks, so a rolling redeploy's brief presence gap doesn't drop a still-deployed
// worker's manifest. The manifests-bucket TTL is the slower backstop.
const workerPresenceManifestReapAfterTicks = 2

// workerPresenceWatcher is the per-process state of runWorkerPresenceWatcher.
type workerPresenceWatcher struct {
	app *App
	log *slog.Logger
	// worker_key → display name, for workers seen live on the previous tick.
	seen   map[string]string
	primed bool
	// worker name → consecutive ticks absent (see workerPresenceManifestReapAfterTicks).
	manifestAbsent map[string]int
	// provider/name of live workers already reported as degraded.
	degraded map[string]bool
}

// tick scans presence, notifies offline transitions and reaps stale manifests.
func (w *workerPresenceWatcher) tick(ctx context.Context) {
	live := w.scan(ctx)
	if live == nil {
		return // KV blip; keep prior state
	}
	if !w.primed {
		w.seen = live
		w.primed = true
		return
	}
	for key, name := range w.seen {
		if _, stillLive := live[key]; !stillLive {
			w.log.Info("worker went offline", "worker_key", key, "worker", name)
			if err := w.app.NotifyWorkerOffline.Execute(ctx, key, name); err != nil {
				w.log.Warn("notify worker offline failed", "worker_key", key, "error", err)
			}
			publishAdminEvent(w.app, domain.NewAdminEvent(domain.AdminEventWorkerOffline, domain.AuditWarn, "admin.worker_offline").AboutWorker(name).With("worker_key", key))
		}
	}
	for key, name := range live {
		if _, known := w.seen[key]; !known {
			publishAdminEvent(w.app, domain.NewAdminEvent(domain.AdminEventWorkerRegistered, domain.AuditInfo, "admin.worker_registered").AboutWorker(name).With("worker_key", key))
		}
	}
	w.noteDegraded(ctx)
	// Manifest keys are worker names ({name}); presence values are the
	// same names. Reap manifests whose worker is no longer live.
	liveNames := make(map[string]struct{}, len(live))
	for _, name := range live {
		liveNames[name] = struct{}{}
	}
	w.reapStaleManifests(ctx, liveNames)
	w.seen = live
}

// reapStaleManifests deletes manifest keys whose worker has no live
// presence. Without this a decommissioned worker's manifest lingers
// forever (no natural writer to expire it) and replays through the
// update-available watcher on every server restart. Delete tombstones are
// ignored by the manifest watcher, so this drives no spurious re-imports.
func (w *workerPresenceWatcher) reapStaleManifests(ctx context.Context, liveNames map[string]struct{}) {
	kv, err := w.app.NATSBus.KV("cairn_worker_manifests")
	if err != nil {
		w.log.Warn("reap stale manifests: manifests bucket unavailable", "error", err)
		return
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		w.log.Debug("reap stale manifests: no keys this tick", "error", err)
		return
	}
	for _, name := range keys {
		if _, live := liveNames[name]; live {
			delete(w.manifestAbsent, name)
			continue
		}
		w.manifestAbsent[name]++
		if w.manifestAbsent[name] < workerPresenceManifestReapAfterTicks {
			continue
		}
		if err := kv.Delete(ctx, name); err != nil {
			w.log.Warn("reap stale manifest failed", "worker", name, "error", err)
			continue
		}
		delete(w.manifestAbsent, name)
		w.log.Info("reaped stale worker manifest (no live presence)", "worker", name)
	}
}

// scan returns worker_key → name for live workers, or nil when the KV is unavailable.
func (w *workerPresenceWatcher) scan(ctx context.Context) map[string]string {
	live := map[string]string{}
	kv, err := w.app.NATSBus.KV("cairn_worker_presence")
	if err != nil {
		return nil // KV unavailable this tick; don't treat as "all offline"
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		return nil
	}
	now := time.Now()
	for _, k := range keys {
		entry, err := kv.Get(ctx, k)
		if err != nil {
			// Presence keys carry a TTL; one can expire between Keys and Get.
			w.log.Debug("presence scan: key vanished", "key", k, "error", err)
			continue
		}
		if key, name, ok := workerPresenceParse(entry.Value, now); ok {
			live[key] = name
		}
	}
	return live
}

// workerPresenceParse decodes a heartbeat; ok is false when invalid or stale.
func workerPresenceParse(value []byte, now time.Time) (key, name string, ok bool) {
	var hb struct {
		WorkerName string `json:"worker_name"`
		WorkerKey  string `json:"worker_key"`
		LastSeen   string `json:"last_seen"`
	}
	if json.Unmarshal(value, &hb) != nil || hb.WorkerKey == "" {
		return "", "", false
	}
	if ts, perr := time.Parse(time.RFC3339, hb.LastSeen); perr == nil && now.Sub(ts) > presenceStaleAfter {
		return "", "", false // stale; treat as not-live
	}
	name = hb.WorkerName
	if name == "" {
		name = hb.WorkerKey
	}
	return hb.WorkerKey, name, true
}
