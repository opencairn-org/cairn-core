package notification

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// DeliverNotifications fans freshly-created notifications out to a user's
// opt-in side-channels (the in-app feed is written separately and always on).
//
//   - EMAIL: resolves the user's (type, email) preference (explicit row, else
//     domain.DefaultChannelEnabled), checks min-severity, sends via EmailSender.
//   - WEBHOOK: POSTs a signed JSON payload to every enabled webhook_endpoint
//     that subscribes to the notification's type+severity (HMAC-SHA256 over
//     the body), and records delivery telemetry / auto-disables dead URLs.
//
// Best-effort and side-band: every failure is logged, never propagated to the
// ingest pipeline. Both channels are independently nil-tolerant so the use case
// works in single-process mode (no email) or before webhooks are wired.
// (quiet hours + per-event delivery audit are the remaining follow-ups.)
type DeliverNotifications struct {
	users         port.UserReader
	notifications port.NotificationRepo // reload an event when retrying a failed delivery
	prefs         port.NotificationPreferenceRepo
	email         port.EmailSender
	webhooks      port.WebhookEndpointRepo
	quietHours    port.QuietHoursRepo
	deliveries    port.NotificationDeliveryRepo
	sender        port.WebhookSender
	now           func() time.Time
	publicURL     string
	logger        *slog.Logger
}

func NewDeliverNotifications(
	users port.UserReader,
	notifications port.NotificationRepo,
	prefs port.NotificationPreferenceRepo,
	email port.EmailSender,
	webhooks port.WebhookEndpointRepo,
	sender port.WebhookSender,
	quietHours port.QuietHoursRepo,
	deliveries port.NotificationDeliveryRepo,
	publicURL string,
	logger *slog.Logger,
) *DeliverNotifications {
	if logger == nil {
		logger = slog.Default()
	}
	return &DeliverNotifications{
		users:         users,
		notifications: notifications,
		prefs:         prefs,
		email:         email,
		webhooks:      webhooks,
		quietHours:    quietHours,
		deliveries:    deliveries,
		sender:        sender,
		now:           time.Now,
		publicURL:     strings.TrimRight(publicURL, "/"),
		logger:        logger.With("component", "deliver_notifications"),
	}
}

// record writes a best-effort delivery-audit row. Nil repo / empty event id
// (e.g. a synthesised test notification) are no-ops.
func (uc *DeliverNotifications) record(ctx context.Context, d domain.NotificationDelivery) {
	if uc.deliveries == nil || d.EventID == (domain.NotificationID{}) {
		return
	}
	d.AttemptedAt = uc.now().UTC()
	// A retryable failure on the first attempt schedules the retry processor:
	// stamp next_retry_at so ListDueRetries can find it.
	if d.Status == domain.DeliveryStatusFailedRetryable {
		if d.Attempt <= 0 {
			d.Attempt = 1
		}
		if t, ok := domain.NextDeliveryRetryAt(d.Attempt, d.AttemptedAt); ok {
			d.NextRetryAt = &t
		} else {
			d.Status = domain.DeliveryStatusFailedPermanent
		}
	}
	if err := uc.deliveries.Record(ctx, d); err != nil {
		uc.logger.Warn("deliver: record audit failed", "event", d.EventID, "channel", d.Channel, "error", err)
	}
}

// quietCache memoises per-user quiet hours for one Execute batch.
type quietCache struct {
	uc     *DeliverNotifications
	loaded map[domain.UserID]domain.QuietHours
}

func (uc *DeliverNotifications) newQuietCache() *quietCache {
	return &quietCache{uc: uc, loaded: map[domain.UserID]domain.QuietHours{}}
}

// suppressed reports whether quiet hours should hold back this notification's
// side-channel delivery. Error-severity notifications (connection broken,
// worker offline) are actionable and NEVER suppressed; info/warn are.
func (c *quietCache) suppressed(ctx context.Context, n domain.Notification) bool {
	if c.uc.quietHours == nil || n.Severity == domain.NotificationSeverityError {
		return false
	}
	q, ok := c.loaded[n.UserID]
	if !ok {
		loaded, err := c.uc.quietHours.Get(ctx, n.UserID)
		if err != nil {
			c.uc.logger.Warn("deliver: load quiet hours failed", "user", n.UserID, "error", err)
			loaded = domain.QuietHours{} // disabled
		}
		q = loaded
		c.loaded[n.UserID] = q
	}
	return q.Suppresses(c.uc.now())
}

// Execute delivers a batch (typically all for one user, from one dispatch).
func (uc *DeliverNotifications) Execute(ctx context.Context, notifs []domain.Notification) {
	if uc == nil || len(notifs) == 0 {
		return
	}
	quiet := uc.newQuietCache()
	if uc.email != nil {
		uc.deliverEmail(ctx, notifs, quiet)
	}
	if uc.webhooks != nil {
		uc.deliverWebhooks(ctx, notifs, quiet)
	}
}

func (uc *DeliverNotifications) deliverEmail(ctx context.Context, notifs []domain.Notification, quiet *quietCache) {
	// Cache per-user prefs + user so a batch hits the DB once per user.
	prefCache := map[domain.UserID]map[prefKey]domain.NotificationPreference{}
	userCache := map[domain.UserID]domain.User{}

	for _, n := range notifs {
		if quiet.suppressed(ctx, n) {
			uc.record(ctx, domain.NotificationDelivery{
				EventID: n.ID, UserID: n.UserID, Channel: domain.NotificationChannelEmail,
				Status: domain.DeliveryStatusSuppressedQuietHours,
			})
			continue
		}
		prefs, ok := prefCache[n.UserID]
		if !ok {
			prefs = uc.loadPrefs(ctx, n.UserID)
			prefCache[n.UserID] = prefs
		}
		if !emailEnabledFor(prefs, n) {
			uc.record(ctx, domain.NotificationDelivery{
				EventID: n.ID, UserID: n.UserID, Channel: domain.NotificationChannelEmail,
				Status: domain.DeliveryStatusSuppressedPreference,
			})
			continue
		}
		user, ok := uc.deliverEmailUser(ctx, userCache, n.UserID)
		if !ok || user.Email == "" {
			continue
		}
		subject, text := uc.render(n)
		d := domain.NotificationDelivery{
			EventID: n.ID, UserID: n.UserID, Channel: domain.NotificationChannelEmail, EmailAddress: user.Email,
		}
		if err := uc.email.Send(ctx, port.EmailMessage{To: user.Email, Subject: subject, TextBody: text}); err != nil {
			uc.logger.Warn("deliver: email send failed", "user", n.UserID, "error", err)
			d.Status = domain.DeliveryStatusFailedRetryable
			d.ErrorMessage = err.Error()
		} else {
			d.Status = domain.DeliveryStatusSent
		}
		uc.record(ctx, d)
	}
}

// deliverEmailUser loads a user through the batch cache; false on lookup error.
func (uc *DeliverNotifications) deliverEmailUser(ctx context.Context, cache map[domain.UserID]domain.User, id domain.UserID) (domain.User, bool) {
	if u, ok := cache[id]; ok {
		return u, true
	}
	u, err := uc.users.GetUser(ctx, id)
	if err != nil {
		uc.logger.Warn("deliver: user lookup failed", "user", id, "error", err)
		cache[id] = domain.User{} // negative-cache
		return domain.User{}, false
	}
	cache[id] = u
	return u, true
}

func (uc *DeliverNotifications) deliverWebhooks(ctx context.Context, notifs []domain.Notification, quiet *quietCache) {
	// Load each user's enabled endpoints once per batch.
	epCache := map[domain.UserID][]domain.WebhookEndpoint{}
	for _, n := range notifs {
		if quiet.suppressed(ctx, n) {
			uc.record(ctx, domain.NotificationDelivery{
				EventID: n.ID, UserID: n.UserID, Channel: domain.NotificationChannelWebhook,
				Status: domain.DeliveryStatusSuppressedQuietHours,
			})
			continue
		}
		eps, ok := epCache[n.UserID]
		if !ok {
			loaded, err := uc.webhooks.ListEnabledForUser(ctx, n.UserID)
			if err != nil {
				uc.logger.Warn("deliver: list webhooks failed", "user", n.UserID, "error", err)
				loaded = nil
			}
			eps = loaded
			epCache[n.UserID] = eps
		}
		for _, ep := range eps {
			if !ep.Subscribes(n.Type, n.Severity) {
				continue
			}
			uc.postWebhook(ctx, ep, n)
		}
	}
}

// postWebhook signs and POSTs one notification to one endpoint, then records
// the outcome. Any non-2xx or transport error counts as a failure (the repo
// increments the failure counter and auto-disables past the threshold).
func (uc *DeliverNotifications) postWebhook(ctx context.Context, ep domain.WebhookEndpoint, n domain.Notification) {
	code, derr := uc.sendWebhookOnce(ctx, ep, n)
	uc.recordWebhook(ctx, ep.ID, code, derr)
	uc.recordWebhookAudit(ctx, ep, n, code, derr)
}

// sendWebhookOnce performs one signed POST and returns the HTTP status code (0
// on transport error) plus a non-empty error string on any failure. It records
// nothing — callers decide how to persist the outcome (initial delivery vs
// retry).
func (uc *DeliverNotifications) sendWebhookOnce(ctx context.Context, ep domain.WebhookEndpoint, n domain.Notification) (int, string) {
	body, err := json.Marshal(uc.webhookPayload(n))
	if err != nil {
		return 0, "marshal payload: " + err.Error()
	}

	mac := hmac.New(sha256.New, []byte(ep.SigningSecret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	if uc.sender == nil {
		return 0, "webhook sender unavailable"
	}
	code, err := uc.sender.Send(ctx, ep.URL, body, map[string]string{
		"Content-Type":      "application/json",
		"User-Agent":        "Cairn-Webhook/1",
		"X-Cairn-Event":     n.Type.String(),
		"X-Cairn-Delivery":  n.ID.String(),
		"X-Cairn-Timestamp": strconv.FormatInt(uc.now().UTC().Unix(), 10),
		"X-Cairn-Signature": "sha256=" + sig,
	})
	if err != nil {
		return 0, err.Error()
	}
	if code < 200 || code >= 300 {
		return code, "non-2xx response"
	}
	return code, ""
}

// retryBatchLimit caps how many due deliveries one RetryDue pass handles. Kept
// well under the repo's retryLease window so a full serial batch finishes
// before any claimed row's lease expires.
const retryBatchLimit = 50

// RetryDue re-attempts every side-channel delivery whose next_retry_at has
// elapsed, advancing each row to sent / failed_retryable (rescheduled) /
// failed_permanent. Called periodically by the server's retry scheduler;
// returns counts for logging. No-op unless deliveries + notifications are wired.
func (uc *DeliverNotifications) RetryDue(ctx context.Context) (sent, rescheduled, gaveUp int) {
	if uc == nil || uc.deliveries == nil || uc.notifications == nil {
		return
	}
	due, err := uc.deliveries.ListDueRetries(ctx, uc.now().UTC(), retryBatchLimit)
	if err != nil {
		uc.logger.Warn("retry: list due deliveries failed", "error", err)
		return
	}
	for _, d := range due {
		code, derr, permanent := uc.resend(ctx, d)
		now := uc.now().UTC()
		madeAttempt := d.Attempt + 1
		switch {
		case derr == "" && !permanent:
			uc.updateOutcome(ctx, d.ID, domain.DeliveryStatusSent, madeAttempt, nil, code, "", now)
			sent++
		case permanent:
			uc.updateOutcome(ctx, d.ID, domain.DeliveryStatusFailedPermanent, madeAttempt, nil, code, derr, now)
			gaveUp++
		default:
			if t, ok := domain.NextDeliveryRetryAt(madeAttempt, now); ok {
				uc.updateOutcome(ctx, d.ID, domain.DeliveryStatusFailedRetryable, madeAttempt, &t, code, derr, now)
				rescheduled++
			} else {
				uc.updateOutcome(ctx, d.ID, domain.DeliveryStatusFailedPermanent, madeAttempt, nil, code, derr, now)
				gaveUp++
			}
		}
	}
	if sent+rescheduled+gaveUp > 0 {
		uc.logger.Info("retry: processed due deliveries", "sent", sent, "rescheduled", rescheduled, "gave_up", gaveUp)
	}
	return
}

// resend re-attempts one delivery. Returns the http code (0 for non-http),
// a non-empty error string on failure, and permanent=true when the failure
// can never succeed on retry (event/endpoint gone, channel disabled) — those
// short-circuit straight to failed_permanent.
func (uc *DeliverNotifications) resend(ctx context.Context, d domain.NotificationDelivery) (code int, derr string, permanent bool) {
	n, err := uc.notifications.GetByID(ctx, d.UserID, d.EventID)
	if err != nil {
		return 0, "notification no longer exists", true
	}
	switch d.Channel {
	case domain.NotificationChannelEmail:
		if uc.email == nil || d.EmailAddress == "" {
			return 0, "email channel unavailable", true
		}
		subject, text := uc.render(n)
		if err := uc.email.Send(ctx, port.EmailMessage{To: d.EmailAddress, Subject: subject, TextBody: text}); err != nil {
			return 0, err.Error(), false
		}
		return 0, "", false
	case domain.NotificationChannelWebhook:
		if uc.webhooks == nil || d.WebhookEndpoint == nil {
			return 0, "webhook channel unavailable", true
		}
		ep, err := uc.webhooks.GetByID(ctx, *d.WebhookEndpoint, d.UserID)
		if err != nil {
			return 0, "webhook endpoint no longer exists", true
		}
		if !ep.Enabled {
			return 0, "webhook endpoint disabled", true
		}
		code, derr := uc.sendWebhookOnce(ctx, ep, n)
		return code, derr, false
	default:
		// in_app / push are not retried through this path.
		return 0, "channel not retryable", true
	}
}

func (uc *DeliverNotifications) updateOutcome(ctx context.Context, id domain.NotificationDeliveryID, status domain.DeliveryStatus, attempt int, next *time.Time, code int, errMsg string, at time.Time) {
	if err := uc.deliveries.UpdateOutcome(ctx, id, status, attempt, next, code, errMsg, at); err != nil {
		uc.logger.Warn("retry: update outcome failed", "delivery", id, "error", err)
	}
}

// recordWebhookAudit writes the per-event delivery-audit row for a webhook POST.
func (uc *DeliverNotifications) recordWebhookAudit(ctx context.Context, ep domain.WebhookEndpoint, n domain.Notification, code int, derr string) {
	epID := ep.ID
	status := domain.DeliveryStatusSent
	if derr != "" || code < 200 || code >= 300 {
		status = domain.DeliveryStatusFailedRetryable
	}
	uc.record(ctx, domain.NotificationDelivery{
		EventID: n.ID, UserID: n.UserID, Channel: domain.NotificationChannelWebhook,
		WebhookEndpoint: &epID, Status: status, HTTPStatusCode: code, ErrorMessage: derr,
	})
}

func (uc *DeliverNotifications) recordWebhook(ctx context.Context, id domain.WebhookEndpointID, code int, derr string) {
	if err := uc.webhooks.RecordDeliveryResult(ctx, id, code, derr, uc.now().UTC()); err != nil {
		uc.logger.Warn("deliver: record webhook result failed", "endpoint", id, "error", err)
	}
}

// webhookPayload is the JSON body POSTed to subscriber endpoints. Stable,
// documented shape — third-party automations parse this.
func (uc *DeliverNotifications) webhookPayload(n domain.Notification) map[string]any {
	link := uc.publicURL + "/notifications"
	p := map[string]any{
		"id":         n.ID.String(),
		"type":       n.Type.String(),
		"severity":   string(n.Severity),
		"title_key":  n.TitleI18nKey,
		"body_key":   n.BodyI18nKey,
		"params":     n.I18nParams,
		"created_at": n.CreatedAt.UTC().Format(time.RFC3339),
	}
	if n.ActivityID != nil {
		p["activity_id"] = n.ActivityID.String()
		link = uc.publicURL + "/activities/" + n.ActivityID.String()
	}
	if n.SegmentID != nil {
		p["segment_id"] = n.SegmentID.String()
	}
	if n.ExternalAccountID != nil {
		p["external_account_id"] = n.ExternalAccountID.String()
	}
	p["link"] = link
	return p
}

type prefKey struct {
	t  domain.NotificationType
	ch domain.NotificationChannel
}

func (uc *DeliverNotifications) loadPrefs(ctx context.Context, userID domain.UserID) map[prefKey]domain.NotificationPreference {
	rows, err := uc.prefs.ListForUser(ctx, userID)
	if err != nil {
		uc.logger.Warn("deliver: load prefs failed", "user", userID, "error", err)
		return nil
	}
	m := make(map[prefKey]domain.NotificationPreference, len(rows))
	for _, p := range rows {
		m[prefKey{p.Type, p.Channel}] = p
	}
	return m
}

// emailEnabledFor resolves whether to email this notification: an explicit
// preference row wins (respecting its min_severity), otherwise the per-type
// default applies.
func emailEnabledFor(prefs map[prefKey]domain.NotificationPreference, n domain.Notification) bool {
	if p, ok := prefs[prefKey{n.Type, domain.NotificationChannelEmail}]; ok {
		return p.Enabled && severityRank(n.Severity) >= severityRank(p.MinSeverity)
	}
	return domain.DefaultChannelEnabled(n.Type, domain.NotificationChannelEmail)
}

func severityRank(s domain.NotificationSeverity) int {
	switch s {
	case domain.NotificationSeverityError:
		return 2
	case domain.NotificationSeverityWarn:
		return 1
	default:
		return 0
	}
}

// render produces a plain-text subject + body. Notifications carry i18n KEYS
// (the SPA localises the in-app feed), so the email uses a compact server-side
// English rendering keyed on the type, plus a deep link.
func (uc *DeliverNotifications) render(n domain.Notification) (subject, body string) {
	link := uc.publicURL + "/notifications"
	if n.ActivityID != nil {
		link = uc.publicURL + "/activities/" + n.ActivityID.String()
	}
	var headline string
	switch n.Type {
	case domain.NotificationTypeSegmentPersonalRecord:
		headline = "You set a personal record on a segment!"
	case domain.NotificationTypeSegmentInstanceRecord:
		headline = "You set a course record on a segment!"
	case domain.NotificationTypeActivityImported:
		headline = "A new activity was imported."
	case domain.NotificationTypeActivityReimported:
		headline = "An activity was updated from its source."
	case domain.NotificationTypeWorkerOffline:
		headline = "A connector (worker) went offline."
	case domain.NotificationTypeExternalAccountRefreshFailed:
		headline = "A connection needs to be re-authorised."
	case domain.NotificationTypeGearDecisionNeeded:
		headline = "A connection reported gear Cairn does not know yet. Create it or map it to existing gear."
	default:
		headline = "You have a new Cairn notification."
	}
	subject = "Cairn: " + headline
	body = fmt.Sprintf("%s\n\nOpen Cairn: %s\n\nManage which notifications email you in Settings.\n\n- Cairn", headline, link)
	return subject, body
}
