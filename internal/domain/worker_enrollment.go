package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// WorkerEnrollment
//
// An admin-issued offer to enroll a worker. Holds only the sha256 hash of
// the plaintext token — the plaintext is shown to the admin exactly once
// at creation time and never persisted server-side. The worker's
// container picks it up from CAIRN_WORKER_ENROLLMENT_TOKEN and sends it
// on every NATS connect.
//
// Mirrors the worker_enrollments table from migration 14.
// ---------------------------------------------------------------------------

type WorkerEnrollment struct {
	ID        WorkerEnrollmentID
	TokenHash []byte // sha256(token), 32 bytes — never the plain token
	// Name is the admin-defined worker identity (NATS-safe, [a-z0-9-]). It is
	// NOT self-set by the worker — the worker must present CAIRN_WORKER_NAME ==
	// Name at connect. Combined with Provider it forms the WorkerKey().
	Name string
	// Provider scopes the worker's NATS permissions; required since v0.3.1.
	// Older rows may hold "" and are refused at connect (NeedsReenroll).
	Provider string
	// Version is the worker's schema version — a simple incrementing integer,
	// part of the admission identity (provider, name, version). Bumping it makes
	// a NEW worker that needs a NEW enrollment; the routing key {name}-{provider}
	// is unaffected so rolling upgrades share the work queue. Defaults to 1.
	Version int
	// WorkerNamePattern is the legacy glob (superseded by exact Name). Retained
	// for back-compat of older rows; new enrollments set Name and leave this "*".
	WorkerNamePattern  string
	PermissionTemplate string // "standard" in v1; future per-enrollment custom perms

	CreatedAt       time.Time
	CreatedByUserID *UserID
	Note            string

	ExpiresAt time.Time
	MaxUses   int // 0 = unlimited until expires/revoked
	Uses      int

	RevokedAt       *time.Time
	RevokedByUserID *UserID
	RevokedReason   string
}

// IsRevoked reports whether the enrollment has been administratively
// revoked. Independent of expiry / use-count exhaustion.
func (e WorkerEnrollment) IsRevoked() bool {
	return e.RevokedAt != nil
}

// IsExpired reports whether `now` is past the enrollment's expiry.
func (e WorkerEnrollment) IsExpired(now time.Time) bool {
	return !now.Before(e.ExpiresAt)
}

// IsExhausted reports whether the enrollment has reached its max-uses limit.
// Returns false for MaxUses == 0 (unlimited).
func (e WorkerEnrollment) IsExhausted() bool {
	if e.MaxUses == 0 {
		return false
	}
	return e.Uses >= e.MaxUses
}

// IsValidAt returns nil when the enrollment can still admit a connection
// at `now`. Returns a descriptive sentinel error otherwise — the
// auth-callout handler distinguishes them in metrics and logs.
func (e WorkerEnrollment) IsValidAt(now time.Time) error {
	switch {
	case e.IsRevoked():
		return ErrEnrollmentRevoked
	case e.IsExpired(now):
		return ErrEnrollmentExpired
	case e.IsExhausted():
		return ErrEnrollmentExhausted
	}
	return nil
}

// NeedsReenroll reports whether the enrollment predates the provider requirement;
// the auth-callout refuses it, so the worker needs a new enrollment.
func (e WorkerEnrollment) NeedsReenroll() bool {
	return e.Provider == ""
}

// ValidProviderID reports whether s is a non-empty NATS-subject-safe provider
// id ([a-z0-9_-]+, as the worker_enrollments CHECK constraint).
func ValidProviderID(s string) bool {
	if s == "" || len(s) > 48 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// WorkerKey is the composite identity/routing token {name}-{provider} used in
// NATS subjects and the webhook URL (e.g. "worker1-strava"). Both halves are
// NATS-safe so the key is too. Falls back to the legacy provider-only key when
// no explicit Name is set (older rows / anonymous dev workers).
func (e WorkerEnrollment) WorkerKey() string {
	return WorkerKey(e.Name, e.Provider)
}

// WorkerKey composes an admin name and provider into the routing token. When
// name is empty it returns just the provider (legacy / unnamed fallback).
func WorkerKey(name, provider string) string {
	if name == "" {
		return provider
	}
	return name + "-" + provider
}

// MatchesWorkerName checks whether the worker's CONNECT-name matches this
// enrollment. The name is now exact and admin-defined; older rows that only
// have a glob pattern fall back to pattern matching.
func (e WorkerEnrollment) MatchesWorkerName(name string) bool {
	if e.Name != "" {
		return e.Name == name
	}
	if e.WorkerNamePattern == "*" || e.WorkerNamePattern == "" {
		return true
	}
	// Trivial prefix-wildcard support: "strava-*" matches "strava-fetcher"
	if strings.HasSuffix(e.WorkerNamePattern, "*") {
		prefix := strings.TrimSuffix(e.WorkerNamePattern, "*")
		return strings.HasPrefix(name, prefix)
	}
	return e.WorkerNamePattern == name
}

// ValidWorkerName reports whether s is a non-empty NATS-subject-safe worker
// name: lowercase letters, digits, hyphens; no leading/trailing hyphen.
func ValidWorkerName(s string) bool {
	if s == "" || len(s) > 48 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// Sentinel errors returned from IsValidAt. Defined here rather than in
// errors.go so they sit next to the type that returns them.
var (
	ErrEnrollmentRevoked          = errors.New("worker enrollment revoked")
	ErrEnrollmentExpired          = errors.New("worker enrollment expired")
	ErrEnrollmentExhausted        = errors.New("worker enrollment exhausted (max_uses reached)")
	ErrEnrollmentNotFound         = errors.New("worker enrollment not found")
	ErrEnrollmentNameMismatch     = errors.New("worker name does not match enrollment pattern")
	ErrEnrollmentProviderMismatch = errors.New("worker provider does not match enrollment provider")
	ErrEnrollmentMissingProvider  = errors.New("worker enrollment has no provider")
)

// ---------------------------------------------------------------------------
// WorkerCredentialGrant
//
// Audit-trail row for each successful auth-callout admission. Created
// when the auth-callout handler validates an enrollment and mints a
// user-JWT. Lookup by user_nkey_public during subsequent reconnect/refresh.
//
// We do NOT store the JWT itself — the worker holds it in-process and
// uses it to authenticate every NATS message. We DO store the nkey
// public form because that's what we need to identify+revoke the grant
// at any later point.
// ---------------------------------------------------------------------------

type WorkerCredentialGrant struct {
	ID           WorkerCredentialGrantID
	EnrollmentID WorkerEnrollmentID

	UserNKeyPublic string // "U..." NATS-encoded Ed25519 public key

	WorkerName       string
	WorkerInstanceID string
	WorkerVersion    string
	ClientHost       string // textual IP, may be empty if NATS didn't report it

	IssuedAt   time.Time
	ExpiresAt  time.Time
	LastSeenAt *time.Time

	RevokedAt       *time.Time
	RevokedByUserID *UserID
	RevokedReason   string
}

// IsValid reports whether the grant currently admits the worker.
func (g WorkerCredentialGrant) IsValid(now time.Time) bool {
	if g.RevokedAt != nil {
		return false
	}
	return now.Before(g.ExpiresAt)
}

// ---------------------------------------------------------------------------
// WorkerPermissionTemplate
//
// What the auth-callout encodes into a user-JWT. The "standard" template
// is derived from the enrollment's provider + worker name, and yields a
// pub/sub list of subjects ending in `.<provider>`. Future templates can
// be admin-defined (e.g. an evaluation worker with read-only permissions).
// ---------------------------------------------------------------------------

type WorkerPermissionTemplate struct {
	Name string // "standard"; matches enrollment.PermissionTemplate

	// AllowPublish is the list of subject patterns this user can publish to.
	AllowPublish []string

	// AllowSubscribe is the list of subject patterns this user can subscribe to.
	AllowSubscribe []string

	// AllowResponseMaxMsgs and AllowResponseTTL are NATS-specific limits
	// for request/reply replies (0 = no limit). The standard template
	// allows unlimited reply messages on _INBOX.>.
	AllowResponseMaxMsgs int
	AllowResponseTTL     time.Duration
}

// JobSubject is the subject core publishes a job on and the named worker
// pool consumes: cairn.jobs.<job>.<provider>.<worker_name> (docs/adr/0006).
// Provider and name are separate tokens, so no two pools overlap on the
// CAIRN_JOBS work queue.
func JobSubject(job, provider, workerName string) string {
	return "cairn.jobs." + job + "." + provider + "." + workerName
}

// DiscoverSubject is the request/reply subject the named pool answers
// discovery on.
func DiscoverSubject(provider, workerName string) string {
	return "cairn.discover." + provider + "." + workerName
}

// WorkerJobTypes are the job kinds a worker pool consumes (provider contract §3).
var WorkerJobTypes = []string{"fetch_source", "parse_blob", "backfill", "reconcile", "import_metrics", "import_gear"}

// StandardPermissionTemplate produces the canonical pub/sub list for a worker.
//
// Three scopes:
//   - Pool-scoped (jobs): keyed on provider AND worker name; core routes
//     each job to one named pool (docs/adr/0006).
//   - Provider-scoped (tokens, results, blobs): shared by the provider's
//     workers.
//   - Worker-scoped (presence, control, webhooks): keyed on the worker key
//     {name}-{provider}, which is also the webhook URL.
//
// JetStream: workers pull jobs and webhook events through durable consumers
// use three KV buckets and read core's info bucket, so the template adds the
// JetStream API subjects for exactly those (see workerJetStreamPublish).
func StandardPermissionTemplate(provider, workerName string) WorkerPermissionTemplate {
	if provider == "" || workerName == "" {
		return WorkerPermissionTemplate{Name: "standard"}
	}
	p := provider
	wk := WorkerKey(workerName, provider)
	jobs := make([]string, 0, 2*len(WorkerJobTypes))
	for _, j := range WorkerJobTypes {
		jobs = append(jobs, JobSubject(j, p, workerName))
	}
	// Pre-0.5 provider-only subjects, so an upgraded pool drains jobs queued
	// before the upgrade. Remove in 0.6.
	for _, j := range WorkerJobTypes {
		jobs = append(jobs, "cairn.jobs."+j+"."+p)
	}
	webhookEvent := fmt.Sprintf("cairn.webhooks.%s.event", wk)
	return WorkerPermissionTemplate{
		Name: "standard",
		AllowPublish: append([]string{
			fmt.Sprintf("cairn.results.fetch_source.%s", p),
			fmt.Sprintf("cairn.results.parse_blob.%s", p),
			fmt.Sprintf("cairn.results.backfill.%s", p),
			fmt.Sprintf("cairn.results.reconcile.%s.*", p), // account-suffixed
			fmt.Sprintf("cairn.results.import_metrics.%s", p),
			fmt.Sprintf("cairn.results.import_gear.%s", p),
			JobSubject("fetch_source", p, workerName), // reconcile/webhook sub-jobs to its own pool
			fmt.Sprintf("cairn.workers.%s.>", wk),
			fmt.Sprintf("cairn.tokens.%s.>", p),
			fmt.Sprintf("cairn.creds.%s.get", p), // custom-credential providers
			fmt.Sprintf("cairn.blobs.presign_upload.%s", p),
			fmt.Sprintf("cairn.blobs.presign_download.%s", p),
			"cairn.accounts.lookup_by_provider_ext", // webhook → account resolution
			"_INBOX.>",                              // request/reply replies the worker initiates
		}, workerJetStreamPublish(jobs, webhookEvent)...),
		AllowSubscribe: append(slices.Clone(jobs),
			fmt.Sprintf("cairn.cmd.worker.%s.>", wk),
			DiscoverSubject(p, workerName),
			webhookEvent,
			fmt.Sprintf("cairn.webhooks.%s.verify", wk),
			"_INBOX.>",
		),
		AllowResponseMaxMsgs: 0, // unlimited
		AllowResponseTTL:     0,
	}
}

// JetStream names the standard template grants access to; they mirror the
// server's bootstrap (internal/adapter/secondary/nats/bootstrap_streams.go).
const (
	WorkerJobStream      = "CAIRN_JOBS"
	WorkerWebhookStream  = "CAIRN_WEBHOOKS"
	WorkerPresenceBucket = "cairn_worker_presence"
	WorkerManifestBucket = "cairn_worker_manifests"
	WorkerRateBucket     = "cairn_rate_limits"
	// CoreInfoBucket holds core's release under key "info"; workers read it.
	CoreInfoBucket = "cairn_core"
)

// workerJetStreamPublish lists the JetStream API subjects a worker needs:
//   - $JS.API.STREAM.NAMES to tell stream from core subjects before publishing
//   - consumer create filtered to its own job/webhook subjects (nats.go), plus
//     the legacy durable create on the job stream (nats-py), info, fetch, ack
//   - info, get and put on the presence, manifest and rate-limit KV buckets
//
// Consumer names stay a wildcard token because the SDKs name durables
// differently, so a worker can address other durables on these two streams.
func workerJetStreamPublish(jobs []string, webhookEvent string) []string {
	out := []string{"$JS.API.STREAM.NAMES"}
	for _, f := range jobs {
		out = append(out, "$JS.API.CONSUMER.CREATE."+WorkerJobStream+".*."+f)
	}
	out = append(out,
		"$JS.API.CONSUMER.DURABLE.CREATE."+WorkerJobStream+".*",
		"$JS.API.CONSUMER.CREATE."+WorkerWebhookStream+".*."+webhookEvent,
	)
	for _, st := range []string{WorkerJobStream, WorkerWebhookStream} {
		out = append(out,
			"$JS.API.CONSUMER.INFO."+st+".*",
			"$JS.API.CONSUMER.MSG.NEXT."+st+".*",
			"$JS.ACK."+st+".>",
		)
	}
	// Core's release, read-only (docs/adr/0005 release check).
	out = append(out,
		"$JS.API.STREAM.INFO.KV_"+CoreInfoBucket,
		"$JS.API.DIRECT.GET.KV_"+CoreInfoBucket+".>",
		"$JS.API.STREAM.MSG.GET.KV_"+CoreInfoBucket,
	)
	for _, b := range []string{WorkerPresenceBucket, WorkerManifestBucket, WorkerRateBucket} {
		out = append(out,
			"$JS.API.STREAM.INFO.KV_"+b,
			"$JS.API.DIRECT.GET.KV_"+b+".>",
			"$JS.API.STREAM.MSG.GET.KV_"+b,
			"$KV."+b+".>",
		)
	}
	return out
}

// ---------------------------------------------------------------------------
// SigningKey
//
// Cairn-server's NATS account NKey. Used to sign worker user-JWTs during
// auth-callout. One row per `purpose` is active at a time; rotation
// happens by inserting a new active row and deactivating the old one
// after NATS-server config is updated to trust both account public keys
// for the overlap period.
// ---------------------------------------------------------------------------

type SigningKey struct {
	ID              SigningKeyID
	Purpose         string // "nats_account" in v1
	PublicKey       string // NKey-encoded ("A..." for accounts)
	SeedEncrypted   []byte // AES-GCM ciphertext of the seed; never logged
	Active          bool
	CreatedAt       time.Time
	CreatedByUserID *UserID
	DeactivatedAt   *time.Time
}

const SigningKeyPurposeNATSAccount = "nats_account"
