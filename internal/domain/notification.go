package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Notification
//
// Mirrors the `notification_events` table. The integer Type field aligns
// with the future cairn.v1.NotificationEventType proto enum — keeping
// the storage as int (rather than a string discriminator) means proto
// enum value renames don't migrate the DB.
//
// Coalescing: when DedupKey is non-empty, a write that finds an
// existing event with the same (user_id, type, dedup_key) within the
// coalescing window (24h in v1) increments coalesce_count instead of
// inserting a new row. The dispatcher computes a stable dedup_key like
// "segment_pr:<segment_id>:YYYY-MM-DD" so hill-repeat workouts produce
// one notification, not seven.
// ---------------------------------------------------------------------------

type Notification struct {
	ID     NotificationID
	UserID UserID

	Type     NotificationType
	Severity NotificationSeverity

	// Title and body are i18n keys. The frontend interpolates I18nParams
	// at render time so the same row renders in any locale.
	TitleI18nKey string
	BodyI18nKey  string
	I18nParams   map[string]string

	// Optional context references for deep-linking.
	ActivityID        *ActivityID
	SegmentID         *SegmentID
	ExternalAccountID *ExternalAccountID
	WorkerName        string

	// DedupKey enables write-time coalescing. Empty = no coalescing
	// (every dispatch inserts a fresh row).
	DedupKey      string
	CoalesceCount int

	Read   bool
	ReadAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NotificationType — integer-backed enum aligned with the proto enum.
// New event types are appended; existing values are stable.
type NotificationType int

const (
	NotificationTypeUnspecified                  NotificationType = 0
	NotificationTypeSegmentPersonalRecord        NotificationType = 1
	NotificationTypeSegmentInstanceRecord        NotificationType = 2
	NotificationTypeActivityImported             NotificationType = 3
	NotificationTypeActivityReimported           NotificationType = 4
	NotificationTypeWorkerOffline                NotificationType = 5
	NotificationTypeExternalAccountRefreshFailed NotificationType = 6
	NotificationTypeGearDecisionNeeded           NotificationType = 7
)

// String returns a stable lowercased identifier for log / wire use.
// Kept in sync with the proto enum's snake_case form.
func (t NotificationType) String() string {
	switch t {
	case NotificationTypeSegmentPersonalRecord:
		return "segment_personal_record"
	case NotificationTypeSegmentInstanceRecord:
		return "segment_instance_record"
	case NotificationTypeActivityImported:
		return "activity_imported"
	case NotificationTypeActivityReimported:
		return "activity_reimported"
	case NotificationTypeWorkerOffline:
		return "worker_offline"
	case NotificationTypeExternalAccountRefreshFailed:
		return "external_account_refresh_failed"
	case NotificationTypeGearDecisionNeeded:
		return "gear_decision_needed"
	default:
		return "unspecified"
	}
}

// NotificationSeverity gates email/push delivery via the
// notification_preferences.min_severity column. The in-app feed
// shows every severity.
type NotificationSeverity string

const (
	NotificationSeverityInfo  NotificationSeverity = "info"
	NotificationSeverityWarn  NotificationSeverity = "warn"
	NotificationSeverityError NotificationSeverity = "error"
)

func (s NotificationSeverity) Valid() bool {
	switch s {
	case NotificationSeverityInfo, NotificationSeverityWarn, NotificationSeverityError:
		return true
	}
	return false
}

// NotificationChannel is a delivery channel for a notification. in_app is the
// always-on feed; email/webhook/push are opt-in side-channels gated by
// per-user NotificationPreference rows.
type NotificationChannel string

const (
	NotificationChannelInApp   NotificationChannel = "in_app"
	NotificationChannelEmail   NotificationChannel = "email"
	NotificationChannelWebhook NotificationChannel = "webhook"
	NotificationChannelPush    NotificationChannel = "push"
)

// NotificationPreference is one cell of the per-user (type × channel) matrix.
// Missing rows resolve to DefaultChannelEnabled.
type NotificationPreference struct {
	UserID      UserID
	Type        NotificationType
	Channel     NotificationChannel
	Enabled     bool
	MinSeverity NotificationSeverity
}

// UserFacingNotificationTypes lists the types a user may configure, in
// display order.
var UserFacingNotificationTypes = []NotificationType{
	NotificationTypeSegmentPersonalRecord, NotificationTypeSegmentInstanceRecord, NotificationTypeActivityImported,
	NotificationTypeActivityReimported, NotificationTypeWorkerOffline, NotificationTypeExternalAccountRefreshFailed,
	NotificationTypeGearDecisionNeeded,
}

// IsUserFacingNotificationType reports whether t is configurable.
func IsUserFacingNotificationType(t NotificationType) bool {
	for _, known := range UserFacingNotificationTypes {
		if t == known {
			return true
		}
	}
	return false
}

// DefaultChannelEnabled is the fallback when a user has no explicit preference
// for a (type, channel): in-app is always on; email defaults on only for the
// "needs attention" types (worker offline, account refresh failed) so routine
// info notifications (PRs, imports) don't email unless the user opts in.
func DefaultChannelEnabled(t NotificationType, ch NotificationChannel) bool {
	switch ch {
	case NotificationChannelInApp:
		return true
	case NotificationChannelEmail:
		switch t {
		case NotificationTypeWorkerOffline, NotificationTypeExternalAccountRefreshFailed:
			return true
		}
		return false
	default:
		return false
	}
}

// MarshalI18nParams returns the params as a JSON object, ready for
// jsonb storage. Empty map serialises to "{}".
func (n Notification) MarshalI18nParams() (json.RawMessage, error) {
	if n.I18nParams == nil {
		return json.RawMessage("{}"), nil
	}
	return json.Marshal(n.I18nParams)
}

// QuietHours is a user's do-not-disturb window for side-channel notification
// delivery (email/webhook). The in-app feed is never suppressed. The window is
// a minute-of-day range [StartMinute, EndMinute) interpreted in TZ, optionally
// restricted to DaysOfWeek (0=Sunday..6=Saturday; empty = every day). A window
// that wraps past midnight (Start > End, e.g. 22:00→07:00) is supported.
type QuietHours struct {
	UserID      UserID
	Enabled     bool
	StartMinute int
	EndMinute   int
	DaysOfWeek  []int
	TZ          string // IANA name, e.g. "Europe/Berlin"
}

// Suppresses reports whether t falls inside the quiet window (in the user's TZ).
// Disabled quiet hours never suppress. An unparseable TZ falls back to UTC.
func (q QuietHours) Suppresses(t time.Time) bool {
	if !q.Enabled {
		return false
	}
	loc, err := time.LoadLocation(q.TZ)
	if err != nil || loc == nil {
		loc = time.UTC
	}
	lt := t.In(loc)
	minute := lt.Hour()*60 + lt.Minute()

	// Day filter: if DaysOfWeek is non-empty, the window only applies on those
	// weekdays. For a wrap-past-midnight window the "day" is the day the window
	// STARTED, so check the start day for the late-evening half and the prior
	// day for the after-midnight half.
	inRange := false
	if q.StartMinute <= q.EndMinute {
		inRange = minute >= q.StartMinute && minute < q.EndMinute
	} else {
		// wraps midnight: in window if after start OR before end
		inRange = minute >= q.StartMinute || minute < q.EndMinute
	}
	if !inRange {
		return false
	}
	if len(q.DaysOfWeek) == 0 {
		return true
	}
	// Determine which calendar day "owns" this window instant.
	day := int(lt.Weekday())
	if q.StartMinute > q.EndMinute && minute < q.EndMinute {
		// after-midnight half belongs to the previous day's window
		day = (day + 6) % 7
	}
	for _, d := range q.DaysOfWeek {
		if d == day {
			return true
		}
	}
	return false
}

// NotificationDeliveryChannelStatus records the outcome of one attempt to
// deliver a notification on one side-channel (notification_deliveries). It is
// the audit trail behind email/webhook delivery.
type NotificationDelivery struct {
	ID              NotificationDeliveryID // set when read back (retry path); zero on initial Record
	EventID         NotificationID
	UserID          UserID
	Channel         NotificationChannel
	WebhookEndpoint *WebhookEndpointID // set for webhook deliveries
	EmailAddress    string             // set for email deliveries
	Status          DeliveryStatus
	HTTPStatusCode  int
	ErrorMessage    string
	Attempt         int
	AttemptedAt     time.Time
	NextRetryAt     *time.Time // set when Status == failed_retryable; nil otherwise
}

// NotificationDeliveryID identifies one notification_deliveries row.
type NotificationDeliveryID uuid.UUID

func (id NotificationDeliveryID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id NotificationDeliveryID) String() string  { return uuid.UUID(id).String() }

// DeliveryStatus mirrors the notification_deliveries.status CHECK constraint.
type DeliveryStatus string

const (
	DeliveryStatusQueued               DeliveryStatus = "queued"
	DeliveryStatusSent                 DeliveryStatus = "sent"
	DeliveryStatusFailedRetryable      DeliveryStatus = "failed_retryable"
	DeliveryStatusFailedPermanent      DeliveryStatus = "failed_permanent"
	DeliveryStatusSuppressedQuietHours DeliveryStatus = "suppressed_quiet_hours"
	DeliveryStatusSuppressedPreference DeliveryStatus = "suppressed_preference"
)

// Notification side-channel (email/webhook) delivery retry policy. A failed
// attempt is rescheduled with this backoff; after the last entry the delivery
// is marked failed_permanent. notificationRetryBackoff[i] is the delay before
// the attempt following the (i+1)-th failed attempt — so the total number of
// attempts is len(notificationRetryBackoff)+1.
var notificationRetryBackoff = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
}

// NotificationMaxDeliveryAttempts is the total number of send attempts (initial
// + retries) before a side-channel delivery is abandoned.
const NotificationMaxDeliveryAttempts = 6

// NextDeliveryRetryAt, given the 1-based number of the attempt that just
// failed, returns when the next attempt should run and whether another attempt
// is allowed at all. ok=false means give up (mark failed_permanent).
func NextDeliveryRetryAt(failedAttempt int, now time.Time) (time.Time, bool) {
	if failedAttempt < 1 {
		failedAttempt = 1
	}
	if failedAttempt > len(notificationRetryBackoff) {
		return time.Time{}, false
	}
	return now.Add(notificationRetryBackoff[failedAttempt-1]), true
}
