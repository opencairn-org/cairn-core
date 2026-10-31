package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ExternalAccountRepo persists and reads external_accounts.
//
// Token columns (access_token_encrypted, refresh_token_encrypted) are
// deliberately not exposed through this interface. The token-fetch
// path lives in a separate use case (FetchProviderToken) that talks
// to a dedicated repository methods FetchTokenForRefresh and
// StoreRefreshedToken — those handle ciphertext-at-rest and never
// expose plaintext outside the use case.
type ExternalAccountRepo interface {
	GetExternalAccount(ctx context.Context, id domain.ExternalAccountID) (domain.ExternalAccount, error)

	// CreateAccount inserts a new external_accounts row (e.g. after a
	// successful OAuth connect) and returns the server-allocated id. If a
	// row for (provider, provider_account_id) already exists for this user
	// it is reused (re-connect), and its id is returned with no error.
	CreateAccount(ctx context.Context, a domain.ExternalAccount) (domain.ExternalAccountID, error)

	ListAccountsForUser(ctx context.Context, userID domain.UserID) ([]domain.ExternalAccount, error)

	// FindByProviderAndExternalID resolves an account by its provider-side
	// id (e.g. Strava's owner_id from a webhook envelope). Returns
	// domain.ErrNotFound when no matching row exists — the webhook
	// handler treats that as "orphaned event, 200-OK and ignore".
	//
	// The same provider account may be linked by several users (the unique
	// index is per user and pool), so a match across users returns
	// domain.ErrAmbiguous instead of an arbitrary pick. A non-empty
	// subscriptionID prefers accounts not bound to another subscription.
	// pool narrows to the connection of that worker pool (docs/adr/0006);
	// "" matches any.
	FindByProviderAndExternalID(ctx context.Context, provider, externalID, subscriptionID, pool string) (domain.ExternalAccount, error)

	// RegisterWebhookSubscription records the provider push-subscription →
	// connection mapping (#50). Several accounts may share one subscription
	// (one per provider app, e.g. Strava).
	RegisterWebhookSubscription(ctx context.Context, provider, subscriptionID string, accountID domain.ExternalAccountID) error

	// FindBySubscription resolves the account behind a provider push
	// subscription. A subscription spans every athlete of the app, so a
	// non-empty ownerID must match provider_account_id too. Several
	// candidates return domain.ErrAmbiguous.
	FindBySubscription(ctx context.Context, provider, subscriptionID, ownerID, pool string) (domain.ExternalAccount, error)

	// HeldByOtherUser reports whether a user other than userID links the
	// provider account (on any app or pool).
	HeldByOtherUser(ctx context.Context, provider, providerAccountID string, userID domain.UserID) (bool, error)

	// SetAutoImport toggles automatic imports (reconcile + webhook fetch) for
	// one registered account (#93). The account stays linked either way.
	SetAutoImport(ctx context.Context, id domain.ExternalAccountID, enabled bool) error
	// SetAssignedWorker pins the account to a named worker pool; "" unpins.
	SetAssignedWorker(ctx context.Context, id domain.ExternalAccountID, workerName string) error

	// DeleteAccount removes the row and what cascades from it: queue, import
	// events, gear and the segments mirrored through it (they cannot exist
	// without an account). Sources and metrics keep their rows with the
	// account link nulled. domain.ErrNotFound when no row matched.
	DeleteAccount(ctx context.Context, id domain.ExternalAccountID) error

	// ListAccountsDueForReconcile returns accounts whose next reconcile
	// is due based on the per-account schedule. Returned accounts are
	// always status=active or status=rate_limited (with reset already
	// past). Caps at `batchSize` so a single tick of the scheduler
	// can't enqueue tens of thousands of jobs in one go.
	//
	// Schedule (implementation-defined, lives in the adapter SQL):
	//   - webhook_subscribed = true:  reconcile every 24h as drift safety
	//   - webhook_subscribed = false: reconcile every N min (default 5)
	//
	// Configurable via instance_settings; the SQL reads the setting and
	// computes the WHERE last_sync_at predicate accordingly.
	ListAccountsDueForReconcile(ctx context.Context, now time.Time, batchSize int) ([]domain.ExternalAccount, error)

	// UpdateSyncWatermark advances the sync_watermark and stamps the
	// sync timestamps. succeeded=true also sets last_successful_sync_at.
	UpdateSyncWatermark(
		ctx context.Context,
		id domain.ExternalAccountID,
		watermark time.Time,
		succeeded bool,
		at time.Time,
	) error

	// UpdateStatus flips the lifecycle status. Used by the reauth-event
	// dispatcher, by the rate-limit-snapshot-updater, and by the manual
	// admin disable command.
	UpdateStatus(
		ctx context.Context,
		id domain.ExternalAccountID,
		status domain.ExternalAccountStatus,
		reason string,
	) error

	// UpdateRateLimit stores the most recent rate-limit snapshot the
	// worker observed. Used by the scheduler to skip accounts in
	// cooldown.
	UpdateRateLimit(
		ctx context.Context,
		id domain.ExternalAccountID,
		snapshot *domain.RateLimitSnapshot,
	) error

	// UpdateAccountConfig shallow-merges the patch into the account's config
	// jsonb. Used for per-connection settings (e.g. poll_interval_seconds).
	UpdateAccountConfig(
		ctx context.Context,
		id domain.ExternalAccountID,
		patch map[string]any,
	) error

	// ListKnownExternalIDs returns the provider-side external_ids of
	// activity sources already imported for the account whose start_time
	// is at or after `since`. Sent with reconcile jobs as known_ext_ids so
	// workers skip re-fetching activities Cairn already holds — without it
	// the watermark's drift margin re-fetches the newest activity on every
	// reconcile tick. All source statuses count (a detached source still
	// dedups to the same row). Newest first, capped at `limit`; a
	// truncated list only costs a few redundant idempotent fetches.
	ListKnownExternalIDs(
		ctx context.Context,
		id domain.ExternalAccountID,
		since time.Time,
		limit int,
	) ([]string, error)
}
