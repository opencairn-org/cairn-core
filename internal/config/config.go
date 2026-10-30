// Package config defines Cairn's runtime configuration and how it is
// loaded from the process environment.
//
// All configuration lives in environment variables prefixed with CAIRN_.
// Nothing is hardcoded — defaults are declared via struct tags and can
// always be overridden. This makes the binary 12-factor-friendly and
// trivial to run under docker-compose, systemd, Kubernetes, or bare metal.
//
// Example:
//
//	CAIRN_DATABASE_URL=postgres://...
//	CAIRN_HTTP_ADDR=0.0.0.0:8080
//	CAIRN_STORAGE_ENDPOINT=https://minio.local
//	CAIRN_AUTH_SESSION_SECRET=...
//
// The flat namespace is deliberate: every variable can be set without
// understanding the internal Go struct shape.
package config

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sethvargo/go-envconfig/v2"
)

// EnvPrefix is the shared prefix for every Cairn environment variable.
const EnvPrefix = "CAIRN"

// Config is the root configuration tree.
//
// All fields are exported and tagged so a single envconfig.ProcessWith call
// populates the whole tree. Validation beyond what the tags express
// (required, default) happens in Validate.
type Config struct {
	Database   DatabaseConfig   `env:", prefix=DATABASE_"`
	HTTP       HTTPConfig       `env:", prefix=HTTP_"`
	NATS       NATSConfig       `env:", prefix=NATS_"`
	Storage    StorageConfig    `env:", prefix=STORAGE_"`
	Auth       AuthConfig       `env:", prefix=AUTH_"`
	Email      EmailConfig      `env:", prefix=EMAIL_"`
	Instance   InstanceConfig   `env:", prefix=INSTANCE_"`
	Log        LogConfig        `env:", prefix=LOG_"`
	Scheduler  SchedulerConfig  `env:", prefix=SCHEDULER_"`
	Geocoder   GeocoderConfig   `env:", prefix=GEOCODER_"`
	Map        MapConfig        `env:", prefix=MAP_"`
	Quota      QuotaConfig      `env:", prefix=QUOTA_"`
	Federation FederationConfig `env:", prefix=FEDERATION_"`
}

// FederationConfig gates ActivityPub federation (docs/federation-design.md).
// Off by default; when on, only users who additionally opt in
// (users.federation_enabled) expose an actor. Phase 1 is read-only discovery.
type FederationConfig struct {
	Enabled bool `env:"ENABLED, default=false"`
	// AllowPrivateHosts drops the SSRF dial guard on federation fetches +
	// deliveries so instances on a private network (LAN self-hosting, or the
	// two-instance interop harness) can reach each other. Leave OFF on the
	// public internet — it re-opens SSRF to internal services.
	AllowPrivateHosts bool `env:"ALLOW_PRIVATE_HOSTS, default=false"`
}

// QuotaConfig holds per-user resource limits (multi-tenant v1). A per-user
// override in the user_quotas table takes precedence over these defaults.
type QuotaConfig struct {
	// MaxActivitiesPerUser caps how many activities one user may own. 0 means
	// unlimited (the default — single-operator instances want no ceiling).
	MaxActivitiesPerUser int `env:"MAX_ACTIVITIES_PER_USER, default=0"`
}

// DatabaseConfig wraps the pgx pool and migration knobs. Mirrors the
// fields on db.Config; the cmd/server wiring copies values across.
type DatabaseConfig struct {
	URL                      string        `env:"URL, required"`
	MaxConns                 int32         `env:"MAX_CONNS, default=10"`
	MinConns                 int32         `env:"MIN_CONNS, default=2"`
	StatementTimeout         time.Duration `env:"STATEMENT_TIMEOUT, default=30s"`
	LockTimeout              time.Duration `env:"LOCK_TIMEOUT, default=5s"`
	IdleInTransactionTimeout time.Duration `env:"IDLE_IN_TX_TIMEOUT, default=60s"`
	ApplicationName          string        `env:"APPLICATION_NAME, default=cairn"`

	// AutoMigrate enables applying pending migrations during `cairn serve`
	// startup. Default false — safer in multi-replica deployments where
	// only one replica should run migrations. For single-node setups
	// flipping this to true gives a one-command upgrade story.
	AutoMigrate bool `env:"AUTO_MIGRATE, default=false"`
}

// HTTPConfig configures the public-facing HTTP server (Connect-RPC + REST).
type HTTPConfig struct {
	Addr            string        `env:"ADDR, default=0.0.0.0:8080"`
	ReadTimeout     time.Duration `env:"READ_TIMEOUT, default=30s"`
	WriteTimeout    time.Duration `env:"WRITE_TIMEOUT, default=5m"`
	IdleTimeout     time.Duration `env:"IDLE_TIMEOUT, default=120s"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT, default=30s"`

	// TLS termination is typically done by a reverse proxy (Caddy / nginx).
	// Setting both cert and key paths enables direct TLS serving for
	// single-binary deployments.
	TLSCertFile string `env:"TLS_CERT_FILE"`
	TLSKeyFile  string `env:"TLS_KEY_FILE"`

	// CORSAllowedOrigins is consulted by the Connect-RPC middleware.
	// Empty list = same-origin only.
	CORSAllowedOrigins []string `env:"CORS_ALLOWED_ORIGINS"`

	// PublicBaseURL is the externally-visible URL the OIDC redirect
	// callback uses. Defaults to "http://localhost:8080" for dev; in
	// production the operator sets this to "https://cairn.example.com".
	PublicBaseURL string `env:"PUBLIC_BASE_URL, default=http://localhost:8080"`
}

// NATSConfig points at the NATS server used as Cairn's worker control
// plane and async job bus. JetStream is required (streams are durable;
// the consumer groups load-balance jobs across worker instances).
//
// Subject layout (production):
//
//	cairn.jobs.<provider>.<job_type>      server → worker dispatch
//	cairn.results.<job_id>                worker → server replies
//	cairn.workers.<name>.<inst>.hb        worker → server heartbeats
//	cairn.workers.register                request-reply registration
//	cairn.events.<topic>                  internal fan-out
//
// JetStream stream names mirror the subject prefixes (CAIRN_JOBS,
// CAIRN_RESULTS, CAIRN_EVENTS). Cairn declares them at startup if they
// don't already exist; operators who pre-provision via `nats stream add`
// just have those calls become no-ops.
type NATSConfig struct {
	URL string `env:"URL, default=nats://localhost:4222"`

	// ClusterName is the cluster the client expects to connect to.
	// Empty disables the check (useful for dev). Production should set
	// this to catch mis-pointing at the wrong NATS deployment.
	ClusterName string `env:"CLUSTER_NAME"`

	// ClientName is the connection's display name in `nats server report
	// connections`. Defaults to "cairn-server"; workers override with
	// their own name.
	ClientName string `env:"CLIENT_NAME, default=cairn-server"`

	// Credentials: either CredsFile (NATS NKey/JWT) or User+Password.
	// Empty means anonymous, suitable only for dev. CredsFile wins if
	// both are set.
	CredsFile string `env:"CREDS_FILE"`
	Username  string `env:"USERNAME"`
	Password  string `env:"PASSWORD"`

	// TLS settings for connecting to NATS over wss/tls. Path-based; an
	// operator who wants in-line PEM would set NATS_URL=tls://... and
	// rely on the system trust store.
	TLSCAFile   string `env:"TLS_CA_FILE"`
	TLSCertFile string `env:"TLS_CERT_FILE"`
	TLSKeyFile  string `env:"TLS_KEY_FILE"`

	// Connection tuning.
	ConnectTimeout time.Duration `env:"CONNECT_TIMEOUT, default=10s"`
	ReconnectWait  time.Duration `env:"RECONNECT_WAIT, default=2s"`
	MaxReconnects  int           `env:"MAX_RECONNECTS, default=-1"` // -1 = forever

	// AckWait is the JetStream ack window per job: a worker has this
	// long to process a job before the broker re-delivers. Should match
	// the slowest expected job (Strava fetch with rate-limit backoff
	// can take a few minutes).
	JobAckWait time.Duration `env:"JOB_ACK_WAIT, default=5m"`

	// JobMaxDeliver caps how many times a failed job is retried before
	// JetStream parks it in the dead-letter consumer. Cairn dispatches
	// a JobFailedFinal notification when this fires.
	JobMaxDeliver int `env:"JOB_MAX_DELIVER, default=5"`

	// AuthCallout makes core answer NATS auth-callout requests, admitting
	// enrolled workers (nats.conf authorization.auth_callout, issuer = the
	// key from POST /api/admin/nats/bootstrap-account-key).
	AuthCallout bool `env:"AUTH_CALLOUT, default=false"`

	// WorkerAccount is the nats.conf account admitted workers are placed in;
	// it must be the account core itself connects to. Required with AuthCallout.
	WorkerAccount string `env:"WORKER_ACCOUNT"`
}

// StorageConfig points at the S3-compatible object store. Defaults assume
// MinIO running on the same host.
//
// Object storage is required for a working instance: since 0.5 every
// event-carrying worker result is claim-checked through it, so without a
// BlobStore provider sync dead-letters, and uploads, photos and cached map
// snapshots are unavailable. Driver=s3 (the default) therefore requires
// credentials and the server refuses to start without them. Driver=none is
// the explicit opt-out for API-only or development instances; it is logged
// at Error and reported as Degraded by the health probe.
type StorageConfig struct {
	// Driver selects the adapter: "s3" (default) or "none".
	Driver string `env:"DRIVER, default=s3"`

	Endpoint        string `env:"ENDPOINT, default=http://minio:9000"`
	Region          string `env:"REGION, default=us-east-1"`
	Bucket          string `env:"BUCKET, default=cairn"`
	AccessKeyID     string `env:"ACCESS_KEY_ID"`
	SecretAccessKey string `env:"SECRET_ACCESS_KEY"`
	UsePathStyle    bool   `env:"USE_PATH_STYLE, default=true"`

	// PresignTTL caps the lifetime of presigned upload/download URLs handed
	// to clients and workers.
	PresignTTL time.Duration `env:"PRESIGN_TTL, default=15m"`

	// PublicEndpoint is the object-store origin clients (browsers, native
	// apps) can reach, used only to presign URLs handed to them; Endpoint
	// stays the cluster-internal address workers and the server use. Empty
	// (the default) means the server serves such content itself through
	// signed /content/{token} URLs, which works on every deployment.
	PublicEndpoint string `env:"PUBLIC_ENDPOINT"`

	// TransferExpiryDays is the lifecycle backstop for claim-checked result
	// payloads under transfer/ — the server deletes them right after ingest,
	// this rule reaps orphans (crashed ingest, envelope never delivered).
	TransferExpiryDays int `env:"TRANSFER_EXPIRY_DAYS, default=1"`
}

// Configured reports whether a BlobStore should be wired: the driver is not
// "none" and credentials are set. An empty Driver counts as "s3" so configs
// built by hand (tests) behave like the env default.
func (s StorageConfig) Configured() bool {
	return !s.Disabled() && s.AccessKeyID != "" && s.SecretAccessKey != ""
}

// Disabled reports the explicit STORAGE_DRIVER=none opt-out.
func (s StorageConfig) Disabled() bool {
	return s.Driver == "none"
}

// MapConfig configures the raster basemap behind server-rendered activity
// snapshots (/api/activities/{id}/map.png). Tiles are fetched once per
// activity+variant and cached in S3, so volume is low. The default is the
// public OSM tile server, whose usage policy requires an identifying
// User-Agent and forbids heavy use — self-hosters with real traffic should
// point TileURL at a keyed or self-hosted provider. The interactive web map is
// configured separately at build time (VITE_MAP_STYLE_URL).
type MapConfig struct {
	// TileURL is a slippy-map template with {z}, {x}, {y} placeholders. 256 px
	// or @2x (512 px) tiles both render correctly.
	TileURL string `env:"TILE_URL, default=https://tile.openstreetmap.org/{z}/{x}/{y}.png"`

	// UserAgent overrides the default identifying User-Agent sent with tile
	// requests (the OSM policy requires one that names the application).
	UserAgent string        `env:"USER_AGENT"`
	Timeout   time.Duration `env:"TIMEOUT, default=15s"`
}

// GeocoderConfig configures reverse-geocoding of activity start locations.
// Defaults point at the OpenStreetMap public Nominatim instance with a
// policy-compliant 1 req/s limit; set URL to a self-hosted Nominatim for
// heavy use, or Enabled=false to disable the feature.
type GeocoderConfig struct {
	Enabled bool   `env:"ENABLED, default=true"`
	URL     string `env:"URL, default=https://nominatim.openstreetmap.org"`

	// Email is the contact address the OSM usage policy asks heavy users to
	// supply. Sent as the `email=` query param and folded into the User-Agent.
	Email string `env:"EMAIL"`

	// UserAgent overrides the default identifying User-Agent. The OSM policy
	// requires a descriptive UA identifying the application.
	UserAgent string `env:"USER_AGENT"`

	// MinInterval is the minimum spacing between requests (1 req/s policy cap).
	MinInterval time.Duration `env:"MIN_INTERVAL, default=1s"`
	Timeout     time.Duration `env:"TIMEOUT, default=10s"`

	// BatchSize is how many un-geocoded activities the backfiller pulls per
	// tick; TickInterval is how often it ticks when work remains. With the
	// 1 req/s limit a batch of 50 takes ~50s, so the tick paces itself.
	BatchSize    int           `env:"BATCH_SIZE, default=50"`
	TickInterval time.Duration `env:"TICK_INTERVAL, default=2m"`
}

// AuthConfig holds session, encryption, and Argon2id parameters.
type AuthConfig struct {
	// SessionSecret signs the session cookie. 32+ random bytes (base64).
	SessionSecret string `env:"SESSION_SECRET, required"`

	// MasterEncryptionKey encrypts secrets at rest (OAuth tokens, OIDC
	// client secrets, webhook signing secrets, etc.). 32 random bytes
	// (base64). Rotating this key requires a re-encryption pass.
	MasterEncryptionKey string `env:"MASTER_ENCRYPTION_KEY, required"`

	SessionTTL     time.Duration `env:"SESSION_TTL, default=720h"`      // 30 days
	SessionIdleMax time.Duration `env:"SESSION_IDLE_MAX, default=168h"` // 7 days

	// Argon2id parameters. Defaults follow OWASP 2024 recommendations.
	Argon2Time    uint32 `env:"ARGON2_TIME, default=2"`
	Argon2Memory  uint32 `env:"ARGON2_MEMORY, default=19456"` // 19 MiB
	Argon2Threads uint8  `env:"ARGON2_THREADS, default=1"`
	Argon2KeyLen  uint32 `env:"ARGON2_KEY_LEN, default=32"`

	// OAuth 2.1 authorization server (native apps, third-party clients, MCP).
	// When disabled, the /oauth/* + discovery endpoints aren't mounted and
	// OAuth access tokens aren't accepted.
	OAuthEnabled         bool          `env:"OAUTH_ENABLED, default=true"`
	OAuthAccessTokenTTL  time.Duration `env:"OAUTH_ACCESS_TOKEN_TTL, default=1h"`
	OAuthRefreshTokenTTL time.Duration `env:"OAUTH_REFRESH_TOKEN_TTL, default=2160h"` // 90 days gitleaks:allow
	OAuthAuthCodeTTL     time.Duration `env:"OAUTH_AUTH_CODE_TTL, default=5m"`
	OAuthAllowDynamicReg bool          `env:"OAUTH_ALLOW_DYNAMIC_REGISTRATION, default=true"`
	// Device authorization (RFC 8628): how long a code pair stays valid and
	// how often a device may poll the token endpoint.
	OAuthDeviceCodeTTL      time.Duration `env:"OAUTH_DEVICE_CODE_TTL, default=10m"`
	OAuthDevicePollInterval time.Duration `env:"OAUTH_DEVICE_POLL_INTERVAL, default=5s"`
}

// EmailConfig selects the outbound email adapter. The adapter type
// determines which sub-fields are required at runtime.
type EmailConfig struct {
	Adapter     string `env:"ADAPTER, default=none"` // none|smtp|ses|resend|mailgun
	FromAddress string `env:"FROM_ADDRESS"`
	FromName    string `env:"FROM_NAME, default=Cairn"`

	// SMTP
	SMTPHost     string `env:"SMTP_HOST"`
	SMTPPort     int    `env:"SMTP_PORT, default=587"`
	SMTPUsername string `env:"SMTP_USERNAME"`
	SMTPPassword string `env:"SMTP_PASSWORD"`
	SMTPStartTLS bool   `env:"SMTP_STARTTLS, default=true"`

	// SES — pulled from STORAGE_* credentials by default if you're in AWS.
	SESRegion string `env:"SES_REGION"`

	// Resend / Mailgun share an API-key pattern.
	APIKey        string `env:"API_KEY"`
	MailgunDomain string `env:"MAILGUN_DOMAIN"`
}

// InstanceConfig holds bootstrap settings that the runtime needs before
// it can read the singleton instance_settings row from the DB.
type InstanceConfig struct {
	// PublicURL is used to construct webhook callback URLs, OAuth redirect
	// URIs, password-reset links, etc.
	PublicURL string `env:"PUBLIC_URL, default=http://localhost:8080"`

	// TrustedProxies is consulted by the IP-extraction middleware.
	TrustedProxies []string `env:"TRUSTED_PROXIES"`

	// BootstrapAdminEmail / BootstrapAdminPassword create a first admin user
	// at startup if no admin exists yet. Optional — the alternative is the
	// `cairn admin create` CLI command.
	BootstrapAdminEmail    string `env:"BOOTSTRAP_ADMIN_EMAIL"`
	BootstrapAdminPassword string `env:"BOOTSTRAP_ADMIN_PASSWORD"`
}

// LogConfig controls the slog handler.
type LogConfig struct {
	Level  string `env:"LEVEL, default=info"`  // debug|info|warn|error
	Format string `env:"FORMAT, default=json"` // json|text
}

// SchedulerConfig configures the background reconciliation loop that
// catches activities missed by webhooks and drives polling for providers
// without webhook support.
//
// The reconciler runs as a goroutine in `cairn serve`. Each tick it
// calls ExternalAccountRepo.ListAccountsDueForReconcile and publishes
// reconcile-jobs to NATS for the returned accounts. The repo SQL filters
// based on:
//
//   - webhook-subscribed accounts: due if last_sync_at > 24h ago (drift safety)
//   - non-webhook accounts:        due if last_sync_at > polling_interval ago
//
// When CAIRN_NATS_URL is unset (no NATS), the scheduler does not start
// at all — the reconcile use case can't publish without a JobBus.
type SchedulerConfig struct {
	// Enabled gates whether the reconciler goroutine starts. Defaults
	// to true; set false for tests or for instances that delegate
	// reconciliation to an external cron + admin endpoint.
	Enabled bool `env:"ENABLED, default=true"`

	// TickInterval is how often the scheduler queries the repo for
	// "accounts due now". Should be much smaller than PollingInterval
	// (the latter is "how stale is acceptable"). 60s default — at most
	// 60s of lag for accounts becoming due, low DB load.
	TickInterval time.Duration `env:"TICK_INTERVAL, default=60s"`

	// PollingInterval is the target reconciliation cadence for
	// non-webhook accounts. The SQL in ListAccountsDueForReconcile
	// returns accounts whose last_sync_at is older than this. Each poll
	// costs at least one provider API read, so the default is hourly —
	// plenty for "new activity appears without a webhook" while leaving
	// the daily read budget to the import queue. Per-connection override
	// via config.poll_interval_seconds.
	PollingInterval time.Duration `env:"POLLING_INTERVAL, default=60m"`

	// WebhookDriftInterval is the drift-safety reconcile cadence for
	// accounts WITH a webhook subscription. Much longer than polling
	// because webhooks cover the real-time path; this only catches
	// misses during provider-side outages.
	WebhookDriftInterval time.Duration `env:"WEBHOOK_DRIFT_INTERVAL, default=24h"`

	// BatchSize caps the per-tick number of accounts scheduled. The
	// tick × batch_size product is the effective reconcile throughput.
	// Default 100 supports ~6000 accounts/hour at the default 60s tick.
	BatchSize int `env:"BATCH_SIZE, default=100"`
}

// Load reads the environment and returns a populated Config, or an error
// from missing/required fields and unparseable values.
func Load() (*Config, error) {
	var cfg Config
	err := envconfig.ProcessWith(context.Background(), &envconfig.Config{
		Target:   &cfg,
		Lookuper: envconfig.PrefixLookuper(EnvPrefix+"_", envconfig.OsLookuper()),
	})
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	for _, v := range envVars(reflect.ValueOf(&cfg).Elem(), EnvPrefix+"_") {
		if v.Required && v.Value.IsZero() {
			return nil, fmt.Errorf("load config: required key %s is empty", v.Key)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

// envVar is one leaf setting as declared by its env struct tag.
type envVar struct {
	Key, Default string
	Required     bool
	Value        reflect.Value
}

// envVars flattens the env tags of a config struct into prefixed keys.
func envVars(v reflect.Value, prefix string) []envVar {
	var out []envVar
	for i := range v.NumField() {
		tag := v.Type().Field(i).Tag.Get("env")
		var ev envVar
		// default= swallows the rest of the tag, as in go-envconfig.
		if before, def, ok := strings.Cut(tag, "default="); ok {
			tag, ev.Default = before, def
		}
		parts := strings.Split(tag, ",")
		for _, opt := range parts[1:] {
			opt = strings.TrimSpace(opt)
			if p, ok := strings.CutPrefix(opt, "prefix="); ok {
				out = append(out, envVars(v.Field(i), prefix+p)...)
			}
			ev.Required = ev.Required || opt == "required"
		}
		if name := strings.TrimSpace(parts[0]); name != "" {
			ev.Key, ev.Value = prefix+name, v.Field(i)
			out = append(out, ev)
		}
	}
	return out
}

// Validate enforces invariants the struct tags cannot express (cross-field rules,
// enum membership, etc.).
func (c *Config) Validate() error {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("LOG_LEVEL must be debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		return fmt.Errorf("LOG_FORMAT must be json|text, got %q", c.Log.Format)
	}

	switch c.Email.Adapter {
	case "none", "smtp", "ses", "resend", "mailgun":
	default:
		return fmt.Errorf("EMAIL_ADAPTER must be none|smtp|ses|resend|mailgun, got %q", c.Email.Adapter)
	}
	if c.Email.Adapter != "none" && c.Email.FromAddress == "" {
		return fmt.Errorf("EMAIL_FROM_ADDRESS required when EMAIL_ADAPTER=%q", c.Email.Adapter)
	}

	if (c.HTTP.TLSCertFile == "") != (c.HTTP.TLSKeyFile == "") {
		return fmt.Errorf("HTTP_TLS_CERT_FILE and HTTP_TLS_KEY_FILE must be set together")
	}

	if c.NATS.URL != "" && c.NATS.AuthCallout {
		if c.NATS.WorkerAccount == "" {
			return fmt.Errorf("NATS_WORKER_ACCOUNT required when NATS_AUTH_CALLOUT=true")
		}
		if strings.ContainsAny(c.NATS.WorkerAccount, " \t\r\n.*>") {
			return fmt.Errorf("NATS_WORKER_ACCOUNT must be a NATS account name, got %q", c.NATS.WorkerAccount)
		}
	}

	if c.Database.MaxConns < c.Database.MinConns {
		return fmt.Errorf("DATABASE_MAX_CONNS (%d) must be >= DATABASE_MIN_CONNS (%d)",
			c.Database.MaxConns, c.Database.MinConns)
	}

	return c.Storage.validate()
}

// validate enforces the driver enum and the credentials the s3 driver needs.
func (s StorageConfig) validate() error {
	switch s.Driver {
	case "", "s3":
		if !s.Configured() {
			return fmt.Errorf("STORAGE_ACCESS_KEY_ID and STORAGE_SECRET_ACCESS_KEY are required with STORAGE_DRIVER=s3; " +
				"set STORAGE_DRIVER=none to run without object storage (provider sync dead-letters, uploads and photos are unavailable)")
		}
	case "none":
	default:
		return fmt.Errorf("STORAGE_DRIVER must be s3|none, got %q", s.Driver)
	}
	return nil
}

// PrintHelp dumps the full env-variable surface to stdout. Used by
// `cairn config help` so operators can see every knob without grepping
// source.
func PrintHelp() error {
	w := tabwriter.NewWriter(os.Stdout, 1, 0, 4, ' ', 0)
	_, _ = fmt.Fprintln(w, "KEY\tTYPE\tDEFAULT\tREQUIRED")
	for _, v := range envVars(reflect.ValueOf(&Config{}).Elem(), EnvPrefix+"_") {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%t\n", v.Key, v.Value.Type(), v.Default, v.Required)
	}
	return w.Flush()
}
