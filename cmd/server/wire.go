package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	emailadapter "github.com/opencairn-org/cairn-core/internal/adapter/secondary/email"
	geocodeadapter "github.com/opencairn-org/cairn-core/internal/adapter/secondary/geocode"
	natsadapter "github.com/opencairn-org/cairn-core/internal/adapter/secondary/nats"
	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/postgres"
	s3adapter "github.com/opencairn-org/cairn-core/internal/adapter/secondary/s3"
	"github.com/opencairn-org/cairn-core/internal/auth"
	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/domain"
	dmatch "github.com/opencairn-org/cairn-core/internal/domain/match"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/admin"
	attachmentuc "github.com/opencairn-org/cairn-core/internal/usecase/attachment"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
	"github.com/opencairn-org/cairn-core/internal/usecase/besteffort"
	"github.com/opencairn-org/cairn-core/internal/usecase/enrollment"
	gearuc "github.com/opencairn-org/cairn-core/internal/usecase/gear"
	geocodeuc "github.com/opencairn-org/cairn-core/internal/usecase/geocode"
	"github.com/opencairn-org/cairn-core/internal/usecase/invite"
	matchuc "github.com/opencairn-org/cairn-core/internal/usecase/match"
	"github.com/opencairn-org/cairn-core/internal/usecase/notification"
	"github.com/opencairn-org/cairn-core/internal/usecase/segment"
	"github.com/opencairn-org/cairn-core/internal/usecase/segmentrank"
	syncuc "github.com/opencairn-org/cairn-core/internal/usecase/sync"
	"github.com/opencairn-org/cairn-core/internal/usecase/trainingload"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// App is the constructed dependency graph the HTTP server uses.
//
// Building it lives in newApp; HTTP handlers (Connect-RPC, REST, GraphQL)
// reach into App's fields to invoke use cases. Keeping construction in
// one place makes the binary's surface obvious and testable — every
// production wire-up goes through here.
type App struct {
	// Adapters
	QuotaMaxActivities int
	// DefaultPollInterval is the instance-wide reconcile cadence
	// (SchedulerConfig.PollingInterval) — surfaced to the UI so the
	// per-connection override setting can say what "instance default" means.
	DefaultPollInterval    time.Duration
	Tx                     *postgres.TxManager
	Activities             *postgres.ActivityRepo
	UserSettings           *postgres.UserSettingsRepo
	Users                  *postgres.UserRepo
	Streams                *postgres.StreamRepo
	BestEfforts            *postgres.BestEffortRepo
	Segments               *postgres.SegmentRepo
	Metrics                *postgres.MetricRepo
	Notifications          port.NotificationRepo // postgres repo wrapped by notification.BroadcastRepo
	NotificationHub        *notification.Hub
	NotificationPrefs      *postgres.NotificationPreferenceRepo
	WebhookEndpoints       *postgres.WebhookEndpointRepo
	QuietHours             *postgres.QuietHoursRepo
	NotificationDeliveries *postgres.NotificationDeliveryRepo
	Follows                *postgres.FollowRepo
	FederationKeys         *postgres.FederationKeyRepo
	FederationActors       *postgres.FederationActorRepo
	FederationFollows      *postgres.FederationFollowRepo
	FederationDeliveries   *postgres.FederationDeliveryRepo
	FederationBlocks       *postgres.FederationBlockRepo
	InboxDedup             *postgres.InboxDedupRepo
	FederationFeed         *postgres.FederationFeedRepo
	Visibility             *postgres.VisibilityRepo
	ShareLinks             *postgres.ShareLinkRepo
	Engagement             *postgres.EngagementRepo
	Blocks                 *postgres.BlockRepo
	Reports                *postgres.ReportRepo
	Moderation             *postgres.ModerationRepo
	Quotas                 *postgres.QuotaRepo
	Clubs                  *postgres.ClubRepo
	ExternalAccounts       *postgres.ExternalAccountRepo
	WorkerEnrollments      *postgres.WorkerEnrollmentRepo
	WorkerPools            port.WorkerPoolRepo
	presence               presenceCache
	SigningKeys            *postgres.SigningKeyRepo
	OAuthTokens            *postgres.OAuthTokenRepo
	DeadLetters            *postgres.DeadLetterRepo
	ImportQueue            port.ImportQueueRepo
	UserProviderConfigs    *postgres.UserProviderConfigRepo
	ImportEvents           *postgres.ImportEventRepo
	AthleteProfiles        *postgres.AthleteProfileRepo
	Passkeys               *postgres.PasskeyRepo
	Sessions               *postgres.SessionRepo
	LinkedIdentities       *postgres.LinkedIdentityRepo

	// OIDCProviders are configured entirely from CAIRN_OIDC_* env vars; there
	// is no client database. Empty means OIDC sign-in is disabled.
	OIDCProviders []domain.OIDCProvider

	// Auth primitives
	PasswordHasher *auth.PasswordHasher
	SecretBox      *auth.SecretBox

	// NATS adapters. All four nil-by-default; populated only when
	// cfg.NATS.URL is set. Wire-order: Bus → SecretBox → CredentialIssuer
	// → AuthCallout subscriber. The RateLimiter sits on top of the
	// Bus's KV handle.
	NATSBus              *natsadapter.Bus
	NATSCredentialIssuer *natsadapter.CredentialIssuer
	NATSAuthCallout      *natsadapter.AuthCalloutSubscriber
	RateLimiter          port.RateLimiter

	// BlobStore is the S3-compatible blob adapter. Nil-by-default; populated
	// only when cfg.Storage.Configured() — i.e. CAIRN_STORAGE_ACCESS_KEY_ID
	// and CAIRN_STORAGE_SECRET_ACCESS_KEY are both set. Code paths that
	// emit presigned URLs (worker blob upload, user-facing raw-download
	// endpoint) check for nil and return Unavailable when the operator
	// hasn't configured storage.
	BlobStore port.BlobStore

	// Email is the outbound email adapter (port.EmailSender). Nil when
	// EMAIL_ADAPTER=none; callers skip the email channel.
	Email port.EmailSender

	// Use cases
	RecomputeActivity  *activity.RecomputeActivityFromSources
	IngestActivity     *activity.IngestActivityFromWorker
	ReclusterBucket    *matchuc.ReclusterBucket
	SourceActions      *activity.SourceActions
	SourceLinks        *matchuc.SourceLinks
	Visibility2        *view.Visibility // shared visibility resolver (REST + Connect)
	Exports            *view.Exports
	Overviews          *view.Overviews
	Searches           *view.Searches
	Feeds              *view.Feeds
	Details            *view.Details
	Stats              *view.Stats
	SegmentViews       *view.Segments
	HealthViews        *view.Health
	Accounts           *view.Accounts
	ClubViews          *view.Clubs
	ProfileViews       *view.Profiles
	AttachmentVariants *attachmentuc.Variants
	// Content mints the signed URLs binary content is delivered under; set by
	// mountContent (needs the HTTP config).
	Content                  port.ContentSigner
	FieldOverrides           port.FieldOverrideRepo
	ClassOverrides           port.ClassificationOverrideRepo
	Attachments              port.AttachmentRepo
	SourceDenylist           port.SourceDenylistRepo
	Health                   port.HealthRepo
	Gear                     *gearuc.Service
	ComputeBestEfforts       *besteffort.ComputeBestEffortsForSource
	MatchSegments            *segment.MatchSegmentsForActivity
	ComputeSegmentRanks      *segmentrank.ComputeSegmentRanks
	ComputeTrainingLoad      *trainingload.ComputeTrainingLoadForUser
	DispatchPR               *notification.DispatchPRNotifications
	DeliverNotifications     *notification.DeliverNotifications
	NotifyAccountNeedsReauth *notification.NotifyAccountNeedsReauth
	NotifyWorkerOffline      *notification.NotifyWorkerOffline
	NotifyGearDecision       *notification.NotifyGearDecision
	ComputeStartPlace        *geocodeuc.ComputeStartPlace
	CreateUser               *admin.CreateUser
	BootstrapAdmin           *admin.BootstrapAdmin
	Invites                  *postgres.InviteRepo
	CreateInvite             *invite.CreateInvite
	RedeemInvite             *invite.RedeemInvite
	PATs                     *postgres.PATRepo
	AuthTokens               *postgres.AuthTokenRepo
	OAuth                    *postgres.OAuthServerRepo

	// ReconcileAccount is nil until a JobBus adapter (NATS) is wired.
	// The reconcile scheduler goroutine in serve.go checks for nil
	// before starting; admin endpoints likewise.
	ReconcileAccount *syncuc.ReconcileExternalAccount

	// Worker-enrollment use cases. CreateWorkerEnrollment + Revoke
	// work without any NATS adapter — the admin can pre-authorise
	// workers and generate tokens even before the auth-callout
	// handler exists. ProcessAuthCallout is built but not yet
	// reachable (needs NATS adapter to subscribe to $SYS.REQ.USER.AUTH).
	CreateWorkerEnrollment *enrollment.CreateWorkerEnrollment
	RevokeWorkerEnrollment *enrollment.RevokeWorkerEnrollment

	// ProcessAuthCallout exists only when the NATS adapter is wired —
	// it depends on the CredentialIssuer for JWT minting.
	ProcessAuthCallout *enrollment.ProcessAuthCallout

	// OIDCLogin is the user-facing OIDC use case. Wired only when at
	// least one oidc_client row is enabled — otherwise the routes
	// stay unregistered.
	OIDCLogin *authuc.OIDCLogin

	// Federation publishing context (Phase 3). PublicBaseURL is the
	// externally-visible origin used to build actor + object URLs;
	// FederationEnabled mirrors the instance flag so the post-ingest
	// publish step can short-circuit when federation is off.
	PublicBaseURL     string
	FederationEnabled bool
	// OAuthEnabled mirrors CAIRN_AUTH_OAUTH_ENABLED for GetInstanceInfo.
	OAuthEnabled bool
}

func newApp(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, logger *slog.Logger) (*App, error) {
	app := &App{
		QuotaMaxActivities:  cfg.Quota.MaxActivitiesPerUser,
		DefaultPollInterval: cfg.Scheduler.PollingInterval,
		PublicBaseURL:       cfg.HTTP.PublicBaseURL,
		OAuthEnabled:        cfg.Auth.OAuthEnabled,
		FederationEnabled:   cfg.Federation.Enabled,
	}
	wireAppCoreRepos(app, pool, cfg)

	// OIDC providers come from CAIRN_OIDC_* env vars (no client DB). Each
	// warning names a provider entry that was skipped due to missing config.
	oidcProviders, oidcWarnings := config.LoadOIDCProviders(os.Getenv)
	for _, w := range oidcWarnings {
		logger.Warn("oidc provider config", "detail", w)
	}
	app.OIDCProviders = oidcProviders

	app.PasswordHasher = auth.NewPasswordHasher(auth.PasswordParams{
		Time:    cfg.Auth.Argon2Time,
		Memory:  cfg.Auth.Argon2Memory,
		Threads: cfg.Auth.Argon2Threads,
		KeyLen:  cfg.Auth.Argon2KeyLen,
	})

	secretBox, err := auth.NewSecretBoxFromMasterKey(cfg.Auth.MasterEncryptionKey)
	if err != nil {
		return nil, err
	}
	app.SecretBox = secretBox

	wireAppSecondaryRepos(app, pool, secretBox)
	// Notifications before ingest: the gear use cases notify on pending
	// provider records.
	if err := wireAppNotifications(app, pool, cfg, secretBox, logger); err != nil {
		return nil, err
	}
	wireAppIngestUseCases(app, pool, logger)
	wireAppAccountUseCases(app, pool)
	if err := wireAppNATS(ctx, app, cfg, logger); err != nil {
		return nil, err
	}

	// OIDC login use case. Constructed only when at least one provider is
	// configured; the HTTP layer gates route registration on OIDCLogin being
	// non-nil.
	if len(oidcProviders) > 0 {
		app.OIDCLogin = authuc.NewOIDCLogin(
			oidcProviders,
			app.LinkedIdentities,
			app.Sessions,
			app.Users,
			cfg.Auth.SessionTTL,
			cfg.HTTP.PublicBaseURL,
		)
	}

	if err := wireAppBlobStore(ctx, app, cfg, logger); err != nil {
		return nil, err
	}
	// After the blob store: variants need it (nil → nothing is derived).
	app.AttachmentVariants = attachmentuc.NewVariants(app.Attachments, app.BlobStore, logger)
	wireAppGeocoder(app, cfg, logger)
	return app, nil
}

// wireAppCoreRepos builds the pool-only postgres repositories.
func wireAppCoreRepos(app *App, pool *pgxpool.Pool, cfg *config.Config) {
	app.Tx = postgres.NewTxManager(pool)
	app.Activities = postgres.NewActivityRepo(pool)
	app.UserSettings = postgres.NewUserSettingsRepo(pool)
	app.Users = postgres.NewUserRepo(pool)
	app.Streams = postgres.NewStreamRepo(pool)
	app.BestEfforts = postgres.NewBestEffortRepo(pool)
	app.Segments = postgres.NewSegmentRepo(pool)
	app.Metrics = postgres.NewMetricRepo(pool)
	app.NotificationHub = notification.NewHub()
	app.Notifications = notification.NewBroadcastRepo(postgres.NewNotificationRepo(pool), app.NotificationHub)
	app.Follows = postgres.NewFollowRepo(pool)
	app.Visibility = postgres.NewVisibilityRepo(pool)
	app.ShareLinks = postgres.NewShareLinkRepo(pool)
	app.Engagement = postgres.NewEngagementRepo(pool)
	app.Blocks = postgres.NewBlockRepo(pool)
	app.Reports = postgres.NewReportRepo(pool)
	app.Moderation = postgres.NewModerationRepo(pool)
	app.Quotas = postgres.NewQuotaRepo(pool)
	app.Clubs = postgres.NewClubRepo(pool)
	app.ExternalAccounts = postgres.NewExternalAccountRepo(
		pool,
		cfg.Scheduler.PollingInterval,
		cfg.Scheduler.WebhookDriftInterval,
	)
	app.WorkerEnrollments = postgres.NewWorkerEnrollmentRepo(pool)
	app.WorkerPools = postgres.NewWorkerPoolRepo(pool)
	app.SigningKeys = postgres.NewSigningKeyRepo(pool)
	app.Sessions = postgres.NewSessionRepo(pool)
	app.LinkedIdentities = postgres.NewLinkedIdentityRepo(pool)
}

// wireAppSecondaryRepos builds the repositories created after the secretbox.
func wireAppSecondaryRepos(app *App, pool *pgxpool.Pool, secretBox *auth.SecretBox) {
	app.OAuthTokens = postgres.NewOAuthTokenRepo(pool, secretBox)
	app.FederationKeys = postgres.NewFederationKeyRepo(pool, secretBox)
	app.FederationActors = postgres.NewFederationActorRepo(pool)
	app.FederationFollows = postgres.NewFederationFollowRepo(pool)
	app.FederationDeliveries = postgres.NewFederationDeliveryRepo(pool)
	app.FederationBlocks = postgres.NewFederationBlockRepo(pool)
	app.InboxDedup = postgres.NewInboxDedupRepo(pool)
	app.FederationFeed = postgres.NewFederationFeedRepo(pool)
	app.DeadLetters = postgres.NewDeadLetterRepo(pool)
	app.ImportQueue = postgres.NewImportQueueRepo(pool)
	app.UserProviderConfigs = postgres.NewUserProviderConfigRepo(pool, secretBox)
	app.ImportEvents = postgres.NewImportEventRepo(pool)
	app.AthleteProfiles = postgres.NewAthleteProfileRepo(pool)
	app.Passkeys = postgres.NewPasskeyRepo(pool)
	app.FieldOverrides = postgres.NewFieldOverrideRepo(pool)
	app.ClassOverrides = postgres.NewClassificationOverrideRepo(pool)
	app.Attachments = postgres.NewAttachmentRepo(pool)
	app.SourceDenylist = postgres.NewSourceDenylistRepo(pool)
	app.Health = postgres.NewHealthRepo(pool)
}

// wireAppIngestUseCases builds the ingest, recluster, gear and post-ingest compute use cases.
func wireAppIngestUseCases(app *App, pool *pgxpool.Pool, logger *slog.Logger) {
	gearRepo := postgres.NewGearRepo(pool)
	app.RecomputeActivity = activity.NewRecomputeActivityFromSources(
		app.Activities,
		app.UserSettings,
		app.AthleteProfiles,
		app.FieldOverrides,
		app.ClassOverrides,
		gearRepo,
		app.Tx,
		nil, // default time.Now
	)
	app.Gear = gearuc.NewService(
		gearRepo,
		app.ExternalAccounts,
		app.Tx,
		func(ctx context.Context, id domain.ActivityID) error {
			_, err := app.RecomputeActivity.Execute(ctx, id)
			return err
		},
		app.NotifyGearDecision.Execute,
		nil, // default time.Now
		nil, // default uuid.NewV7
	)
	app.IngestActivity = activity.NewIngestActivityFromWorker(
		app.Activities,
		app.Streams,
		app.UserSettings,
		app.RecomputeActivity,
		app.Tx,
		nil, // default time.Now
		nil, // default uuid.NewV7
	)
	app.ReclusterBucket = matchuc.NewReclusterBucket(
		app.Activities,
		app.RecomputeActivity,
		app.SourceDenylist,
		app.Tx,
		nil, // default uuid.NewV7
		dmatch.DefaultOptions(),
		logger,
	)
	wireAppViews(app, logger)
	// The owner-facing source actions, shared by REST and Connect.
	app.SourceActions = activity.NewSourceActions(app.Activities, app.SourceDenylist, app.RecomputeActivity, newFederationPublisher(app, logger), nil)
	app.SourceLinks = matchuc.NewSourceLinks(app.Activities, app.ReclusterBucket)
	app.ComputeBestEfforts = besteffort.NewComputeBestEffortsForSource(
		app.Activities,
		app.Streams,
		app.BestEfforts,
		app.Tx,
		nil, // default time.Now
		nil, // default uuid.NewV7
	)
	app.ComputeSegmentRanks = segmentrank.NewComputeSegmentRanks(app.Segments, app.Tx)
	app.MatchSegments = segment.NewMatchSegmentsForActivity(
		app.Activities,
		app.Streams,
		app.Segments,
		app.Tx,
		app.ComputeSegmentRanks,
		nil,                      // default time.Now
		nil,                      // default uuid.NewV7
		domain.MatchTolerances{}, // zero → DefaultMatchTolerances
		nil,                      // default slog.Default
	)
	app.ComputeTrainingLoad = trainingload.NewComputeTrainingLoadForUser(
		app.Activities,
		app.Metrics,
		app.Tx,
		nil, // default time.Now
		nil, // default uuid.NewV7
	)
}

// wireAppEmail builds the email sender (gated on EMAIL_ADAPTER); nil when disabled.
func wireAppEmail(cfg *config.Config, logger *slog.Logger) (port.EmailSender, error) {
	switch cfg.Email.Adapter {
	case "smtp":
		es, err := emailadapter.NewSMTP(cfg.Email)
		if err != nil {
			return nil, err
		}
		logger.Info("email wired", "adapter", "smtp", "host", cfg.Email.SMTPHost, "from", cfg.Email.FromAddress)
		return es, nil
	case "", "none":
		logger.Info("email disabled: EMAIL_ADAPTER=none")
	default:
		logger.Warn("email adapter not implemented; disabling", "adapter", cfg.Email.Adapter)
	}
	return nil, nil
}

// wireAppNotifications builds email + notification side-channel delivery and
// the notification use cases. Built here so DispatchPR can fan PR notifications out to email.
func wireAppNotifications(app *App, pool *pgxpool.Pool, cfg *config.Config, secretBox *auth.SecretBox, logger *slog.Logger) error {
	emailSender, err := wireAppEmail(cfg, logger)
	if err != nil {
		return err
	}
	app.Email = emailSender
	app.NotificationPrefs = postgres.NewNotificationPreferenceRepo(pool)
	app.WebhookEndpoints = postgres.NewWebhookEndpointRepo(pool, secretBox)
	app.QuietHours = postgres.NewQuietHoursRepo(pool)
	app.NotificationDeliveries = postgres.NewNotificationDeliveryRepo(pool)
	app.DeliverNotifications = notification.NewDeliverNotifications(
		app.Users, app.Notifications, app.NotificationPrefs, emailSender, app.WebhookEndpoints, app.QuietHours, app.NotificationDeliveries, cfg.HTTP.PublicBaseURL, logger,
	)

	app.DispatchPR = notification.NewDispatchPRNotifications(
		app.Segments,
		app.Notifications,
		app.Tx,
		app.DeliverNotifications,
		nil, // default time.Now
		nil, // default uuid.NewV7
	)
	app.NotifyAccountNeedsReauth = notification.NewNotifyAccountNeedsReauth(
		app.ExternalAccounts,
		app.Notifications,
		app.Tx,
		app.DeliverNotifications,
		nil, // default time.Now
		nil, // default uuid.NewV7
	)
	app.NotifyWorkerOffline = notification.NewNotifyWorkerOffline(
		app.Users,
		app.Notifications,
		app.Tx,
		app.DeliverNotifications,
		nil, // default time.Now
		nil, // default uuid.NewV7
	)
	app.NotifyGearDecision = notification.NewNotifyGearDecision(
		app.Notifications,
		app.Tx,
		app.DeliverNotifications,
		nil, // default time.Now
		nil, // default uuid.NewV7
	)
	return nil
}

// wireAppAccountUseCases builds user/invite/token repos and use cases plus worker enrollment.
func wireAppAccountUseCases(app *App, pool *pgxpool.Pool) {
	app.CreateUser = admin.NewCreateUser(app.Users, app.PasswordHasher, nil, nil)
	app.BootstrapAdmin = admin.NewBootstrapAdmin(app.Users, app.CreateUser)
	app.Invites = postgres.NewInviteRepo(pool)
	app.CreateInvite = invite.NewCreateInvite(app.Invites, nil)
	app.RedeemInvite = invite.NewRedeemInvite(app.Invites, app.CreateUser, nil)
	app.PATs = postgres.NewPATRepo(pool)
	app.AuthTokens = postgres.NewAuthTokenRepo(pool)
	app.OAuth = postgres.NewOAuthServerRepo(pool)

	// Worker-enrollment use cases. CreateWorkerEnrollment is the
	// admin-side flow ("generate a token I can put in a worker's env").
	// RevokeWorkerEnrollment is the kill-switch. ProcessAuthCallout
	// (built but not constructed here) needs a NATSCredentialIssuer
	// adapter from the NATS layer; we'll wire it once that exists.
	app.CreateWorkerEnrollment = enrollment.NewCreateWorkerEnrollment(
		app.WorkerEnrollments,
		nil, // default time.Now
		nil, // default uuid.NewV7
		nil, // default crypto/rand
	)
	app.RevokeWorkerEnrollment = enrollment.NewRevokeWorkerEnrollment(
		app.WorkerEnrollments,
		nil, // default time.Now
	)
}

// wireAppNATS wires the NATS adapter (gated on cfg.NATS.URL).
//
// When CAIRN_NATS_URL is unset, the bus stays nil and every
// dependent capability (reconcile scheduler, auth-callout subscriber,
// rate limiter, request/reply token-fetch) silently skips. This
// keeps single-binary dev setups runnable without NATS.
//
// When set, we:
//  1. Connect (eager).
//  2. Bootstrap canonical streams + KV/OS buckets (idempotent).
//  3. Construct CredentialIssuer + AuthCalloutSubscriber.
//  4. Build the rate limiter on the cairn_rate_limits KV bucket.
//  5. Build ProcessAuthCallout (use case) on top of the issuer.
//  6. Build ReconcileExternalAccount on top of the bus.
func wireAppNATS(ctx context.Context, app *App, cfg *config.Config, logger *slog.Logger) error {
	if cfg.NATS.URL == "" {
		logger.Info("nats: CAIRN_NATS_URL unset — async layer disabled, single-process mode")
		return nil
	}
	bus, err := natsadapter.NewBus(ctx, cfg.NATS, logger)
	if err != nil {
		return err
	}

	// Stream / KV / Object-Store bootstrap. Idempotent — see
	// docs/architecture.md §2 for the configs.
	if err := bus.BootstrapStreams(ctx); err != nil {
		return err
	}
	// Workers compare their release with this (docs/adr/0005).
	if kv, err := bus.KV(natsadapter.KVCoreInfo); err == nil {
		info, _ := json.Marshal(map[string]string{"release": Version})
		if _, err := kv.Put(ctx, "info", info); err != nil {
			logger.Warn("publish core info failed", "error", err)
		}
	}

	credentialIssuer := natsadapter.NewCredentialIssuer(app.SigningKeys, app.SecretBox, cfg.NATS.WorkerAccount)

	// Build the rate limiter on the rate-limits KV bucket. The
	// server-side limiter is intentionally permissive (nil
	// capacities = no buckets configured = always allow). Workers
	// pass their own per-provider capacities at construction —
	// rate-limit policy is a worker concern, not a core concern.
	rlKV, kvErr := bus.KV(natsadapter.KVRateLimits)
	if kvErr != nil {
		return kvErr
	}

	// Worker auth-callout use case. Needs the issuer.
	processAuth := enrollment.NewProcessAuthCallout(
		app.WorkerEnrollments,
		credentialIssuer,
		app.Tx,
		nil,                 // default time.Now
		nil,                 // default uuid.NewV7
		cfg.NATS.JobAckWait, // reuse as JWT TTL — typically 5min-24h
	)

	app.NATSBus = bus
	app.NATSCredentialIssuer = credentialIssuer
	app.RateLimiter = natsadapter.NewRateLimiter(rlKV, nil)
	app.ProcessAuthCallout = processAuth
	// Start() is invoked from serve.go so the subscription's
	// lifecycle matches the HTTP server's.
	if cfg.NATS.AuthCallout {
		app.NATSAuthCallout = natsadapter.NewAuthCalloutSubscriber(
			bus, processAuth, credentialIssuer, logger,
		)
	}

	// Reconciliation scheduler use case now has a real bus. The budget
	// gate skips accounts whose provider API budget is spent (live KV
	// state the workers sync from real usage headers) — polling an
	// exhausted provider only burns budget the backfill needs.
	app.ReconcileAccount = syncuc.NewReconcileExternalAccount(
		app.ExternalAccounts,
		bus,
		presenceWorkerRouter{app: app, logger: logger},
		func(ctx context.Context, a domain.ExternalAccount, pool string) bool {
			_, exhausted := providerBudget(ctx, bus, a.Provider, accountAppID(ctx, app, a), pool)
			return exhausted
		},
		gearSyncHook(app, logger),
		nil, // default time.Now
		nil, // default uuid.NewV7
	)
	return nil
}

// wireAppBlobStore wires the S3-compatible blob store. Gated on storage
// credentials being set — instances that don't yet have object storage
// configured come up in a "no raw blobs" mode; ingest pipelines still run end-to-end.
func wireAppBlobStore(ctx context.Context, app *App, cfg *config.Config, logger *slog.Logger) error {
	if !cfg.Storage.Configured() {
		logger.Info("blob store not configured: CAIRN_STORAGE_ACCESS_KEY_ID/SECRET_ACCESS_KEY unset")
		return nil
	}
	bs, err := s3adapter.New(cfg.Storage)
	if err != nil {
		return err
	}
	// Create the configured bucket (CAIRN_STORAGE_BUCKET) if it's missing —
	// part of core bootstrap so a fresh deployment needs no out-of-band step.
	if err := bs.EnsureBucket(ctx); err != nil {
		return fmt.Errorf("ensure storage bucket: %w", err)
	}
	if err := bs.EnsureLifecycleRule(ctx, "cairn-transfer-expiry", transferPrefix, wireClampInt32(cfg.Storage.TransferExpiryDays)); err != nil {
		return fmt.Errorf("ensure transfer lifecycle: %w", err)
	}
	app.BlobStore = bs
	logger.Info("blob store wired",
		"endpoint", cfg.Storage.Endpoint,
		"bucket", cfg.Storage.Bucket,
		"path_style", cfg.Storage.UsePathStyle)
	return nil
}

// wireClampInt32 converts n to int32, saturating at the int32 bounds.
func wireClampInt32(n int) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	default:
		return int32(n)
	}
}

// wireAppGeocoder wires the reverse-geocoder for activity start-location
// subtitles. Gated on CAIRN_GEOCODER_ENABLED (default true). The adapter
// enforces the provider's usage policy internally; the backfiller in serve.go drives it.
func wireAppGeocoder(app *App, cfg *config.Config, logger *slog.Logger) {
	if !cfg.Geocoder.Enabled {
		logger.Info("geocoder disabled: CAIRN_GEOCODER_ENABLED=false")
		return
	}
	geocoder := geocodeadapter.New(cfg.Geocoder)
	app.ComputeStartPlace = geocodeuc.NewComputeStartPlace(app.Activities, app.Streams, geocoder, logger)
	logger.Info("geocoder wired", "url", cfg.Geocoder.URL, "min_interval", cfg.Geocoder.MinInterval)
}

// wireAppViews builds the read-side use cases REST and Connect both render from.
func wireAppViews(app *App, logger *slog.Logger) {
	app.Visibility2 = view.NewVisibility(app.Blocks, app.Follows, app.Visibility)
	app.Exports = view.NewExports(app.Streams)
	app.Overviews = view.NewOverviews(app.Activities, nil)
	app.Searches = view.NewSearches(app.Activities, app.Gear)
	var fedFeed port.FederationFeedRepo
	if app.FederationFeed != nil {
		fedFeed = app.FederationFeed
	}
	app.Feeds = view.NewFeeds(app.Activities, app.Users, app.Visibility2, fedFeed, app.FederationEnabled)
	var metrics port.MetricRepo
	if app.Metrics != nil {
		metrics = app.Metrics
	}
	var bestEfforts port.BestEffortRepo
	if app.BestEfforts != nil {
		bestEfforts = app.BestEfforts
	}
	app.Stats = view.NewStats(app.Activities, metrics, bestEfforts, app.Streams, nil)
	app.SegmentViews = view.NewSegments(nilIfNilSegments(app))
	app.HealthViews = view.NewHealth(app.Health, nil)
	var queue port.ImportQueueRepo
	if app.ImportQueue != nil {
		queue = app.ImportQueue
	}
	var events port.ImportEventRepo
	if app.ImportEvents != nil {
		events = app.ImportEvents
	}
	var conns port.UserProviderConfigRepo
	if app.UserProviderConfigs != nil {
		conns = app.UserProviderConfigs
	}
	var clubs port.ClubRepo
	if app.Clubs != nil {
		clubs = app.Clubs
	}
	app.ClubViews = view.NewClubs(clubs, app.Users, app.Feeds)
	app.ProfileViews = view.NewProfiles(app.Users, app.Activities, app.Follows, app.Blocks, app.Visibility2)
	app.Accounts = view.NewAccounts(app.ExternalAccounts, app.Activities, queue, events, conns, syncInsight{app: app, logger: logger}, nil)
	app.Details = view.NewDetails(app.Activities, app.Streams, nilIfNilSegments(app), app.AthleteProfiles, nilIfNilEngagement(app),
		app.Users, app.FieldOverrides, workerPresence{app: app, logger: logger}, app.Visibility2)
}

// nilIfNilSegments / nilIfNilEngagement keep a nil pointer from becoming a
// non-nil interface.
func nilIfNilSegments(app *App) port.SegmentRepo {
	if app.Segments == nil {
		return nil
	}
	return app.Segments
}

func nilIfNilEngagement(app *App) port.EngagementRepo {
	if app.Engagement == nil {
		return nil
	}
	return app.Engagement
}
