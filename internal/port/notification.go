package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// WebhookEndpointRepo persists user-configured outbound webhook endpoints
// (webhook_endpoints, migration 10). The signing secret is encrypted at rest
// with the instance master key; Create/GetByID/ListEnabledForUser return it
// decrypted (callers that don't need it ignore the field), while ListForUser
// blanks it for safe display.
type WebhookEndpointRepo interface {
	// Create inserts a new endpoint (secret encrypted) and returns it.
	Create(ctx context.Context, e domain.WebhookEndpoint) (domain.WebhookEndpoint, error)
	// Update writes name/url/event_types/min_severity/enabled. Re-enabling a
	// previously auto-disabled endpoint (Enabled=true) clears AutoDisabled.
	Update(ctx context.Context, e domain.WebhookEndpoint) error
	// RotateSecret replaces the encrypted signing secret.
	RotateSecret(ctx context.Context, id domain.WebhookEndpointID, userID domain.UserID, newSecret string) error
	// Delete removes an endpoint (scoped to its owner).
	Delete(ctx context.Context, id domain.WebhookEndpointID, userID domain.UserID) error
	// GetByID returns one endpoint (secret decrypted), scoped to its owner.
	GetByID(ctx context.Context, id domain.WebhookEndpointID, userID domain.UserID) (domain.WebhookEndpoint, error)
	// ListForUser returns the user's endpoints WITHOUT the secret (display).
	ListForUser(ctx context.Context, userID domain.UserID) ([]domain.WebhookEndpoint, error)
	// ListEnabledForUser returns enabled, non-auto-disabled endpoints WITH
	// the decrypted secret — the dispatch hot path.
	ListEnabledForUser(ctx context.Context, userID domain.UserID) ([]domain.WebhookEndpoint, error)
	// RecordDeliveryResult updates telemetry after a POST attempt:
	// last_delivery_*, consecutive_failures (reset on success, ++ on fail),
	// and auto_disabled once failures cross the threshold.
	RecordDeliveryResult(ctx context.Context, id domain.WebhookEndpointID, statusCode int, deliveryErr string, at time.Time) error
}

// NotificationPreferenceRepo persists the sparse per-user (type × channel)
// preference matrix (notification_preferences, migration 10). Missing rows
// resolve to domain.DefaultChannelEnabled at the call site.
type NotificationPreferenceRepo interface {
	// ListForUser returns the user's explicit preference rows (no defaults).
	ListForUser(ctx context.Context, userID domain.UserID) ([]domain.NotificationPreference, error)
	// Upsert writes one (type, channel) preference.
	Upsert(ctx context.Context, p domain.NotificationPreference) error
}

// NotificationRepo persists in-app notification events.
//
// Coalescing semantics: when a written notification has a non-empty
// DedupKey, the implementation first looks for an existing event with
// the same (user_id, type, dedup_key) within the coalescing window
// (24h in v1) — on hit, it increments coalesce_count and merges
// i18n_params instead of inserting. Notifications with empty DedupKey
// always insert fresh.
type NotificationRepo interface {
	// SaveNotifications writes a batch. Each row independently goes
	// through the coalesce-or-insert path. Empty input is a no-op.
	SaveNotifications(ctx context.Context, notifs []domain.Notification) error

	// ListNotificationsForUser returns notifications for the user,
	// ordered by created_at DESC, with pagination by offset+limit.
	// limit <= 0 defaults to 50. unreadOnly filters to read=false rows.
	ListNotificationsForUser(
		ctx context.Context,
		userID domain.UserID,
		unreadOnly bool,
		limit, offset int,
	) ([]domain.Notification, error)

	// MarkRead flips notification_events.read = true for the supplied
	// IDs. Caller-scoped: implementations enforce that only events
	// belonging to the supplied userID are mutated.
	MarkRead(
		ctx context.Context,
		userID domain.UserID,
		ids []domain.NotificationID,
	) error

	// MarkAllReadForUser flips read = true for every unread event
	// belonging to the user. Returns the number of rows mutated.
	MarkAllReadForUser(
		ctx context.Context,
		userID domain.UserID,
	) (int, error)

	// CountUnreadForUser counts the user's unread events (the badge number).
	CountUnreadForUser(ctx context.Context, userID domain.UserID) (int, error)

	// GetByID returns a single notification scoped to its owning user.
	// Returns domain.ErrNotFound when the row doesn't exist OR when
	// it belongs to a different user — the two cases are indistinguishable
	// to the caller (privacy: never confirm "this id exists but isn't
	// yours").
	GetByID(
		ctx context.Context,
		userID domain.UserID,
		id domain.NotificationID,
	) (domain.Notification, error)
}

// QuietHoursRepo persists a user's do-not-disturb window (notification_quiet_hours).
type QuietHoursRepo interface {
	// Get returns the user's quiet hours; a zero-value (disabled) QuietHours
	// when no row exists.
	Get(ctx context.Context, userID domain.UserID) (domain.QuietHours, error)
	// Upsert writes the user's quiet hours.
	Upsert(ctx context.Context, q domain.QuietHours) error
}

// NotificationDeliveryRepo records the per-event, per-channel delivery audit
// (notification_deliveries). Best-effort: a failed audit write never blocks
// the actual delivery.
type NotificationDeliveryRepo interface {
	Record(ctx context.Context, d domain.NotificationDelivery) error
	// ListForUser returns recent deliveries for the user, newest first.
	ListForUser(ctx context.Context, userID domain.UserID, limit int) ([]domain.NotificationDelivery, error)

	// ListDueRetries returns failed_retryable deliveries whose next_retry_at
	// has elapsed (oldest first, capped at limit). Each row carries its ID,
	// channel and target so the retry processor can re-send it.
	ListDueRetries(ctx context.Context, now time.Time, limit int) ([]domain.NotificationDelivery, error)

	// UpdateOutcome rewrites a delivery row after a retry attempt: its status,
	// the attempt count, the next retry time (nil to clear), and the latest
	// http code / error / attempted_at.
	UpdateOutcome(
		ctx context.Context,
		id domain.NotificationDeliveryID,
		status domain.DeliveryStatus,
		attempt int,
		nextRetryAt *time.Time,
		httpStatusCode int,
		errorMessage string,
		attemptedAt time.Time,
	) error
}
