package notification

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Settings are the user's side-channel controls: quiet hours, the delivery
// audit and the test email.
type Settings struct {
	QuietHours port.QuietHoursRepo
	Deliveries port.NotificationDeliveryRepo
	Users      port.UserReader
	Mail       port.EmailSender
}

// QuietHoursOf returns the user's do-not-disturb window (disabled when unset).
func (s *Settings) QuietHoursOf(ctx context.Context, userID domain.UserID) (domain.QuietHours, error) {
	return s.QuietHours.Get(ctx, userID)
}

// SetQuietHours stores the user's do-not-disturb window.
func (s *Settings) SetQuietHours(ctx context.Context, q domain.QuietHours) error {
	return s.QuietHours.Upsert(ctx, q)
}

// RecentDeliveries is the user's delivery audit, newest first.
func (s *Settings) RecentDeliveries(ctx context.Context, userID domain.UserID, limit int) ([]domain.NotificationDelivery, error) {
	return s.Deliveries.ListForUser(ctx, userID, limit)
}

// SendTestEmail mails the caller's own address so they can check the channel; it returns the address.
func (s *Settings) SendTestEmail(ctx context.Context, userID domain.UserID) (string, error) {
	if s.Mail == nil {
		return "", port.NewActionError(port.ActionErrorUnavailable, "email is not configured on this instance")
	}
	user, err := s.Users.GetUser(ctx, userID)
	if err != nil {
		return "", port.NewActionError(port.ActionErrorInternal, "user lookup failed")
	}
	if user.Email == "" {
		return "", port.NewActionError(port.ActionErrorInvalid, "your account has no email address")
	}
	err = s.Mail.Send(ctx, port.EmailMessage{
		To:      user.Email,
		Subject: "Cairn — test email",
		TextBody: "This is a test email from your Cairn instance.\n\n" +
			"If you received this, the email channel is working and notifications " +
			"can be delivered to this address.\n\n— Cairn",
		HTMLBody: "<p>This is a <strong>test email</strong> from your Cairn instance.</p>" +
			"<p>If you received this, the email channel is working and notifications " +
			"can be delivered to this address.</p><p>— Cairn</p>",
	})
	if err != nil {
		return "", port.NewActionError(port.ActionErrorUpstream, "send failed: "+err.Error())
	}
	return user.Email, nil
}

// Webhooks manages the user's outbound webhook endpoints. The signing
// secret is returned only by Create and RotateSecret.
type Webhooks struct {
	Repo    port.WebhookEndpointRepo
	Deliver *DeliverNotifications
	Now     func() time.Time
}

func (w *Webhooks) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

// ErrWebhookSecret: no randomness for a signing secret.
var ErrWebhookSecret = errors.New("secret generation failed")

// newWebhookSecret returns a 32-byte URL-safe HMAC signing secret.
func newWebhookSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// List returns the user's endpoints without their secrets.
func (w *Webhooks) List(ctx context.Context, userID domain.UserID) ([]domain.WebhookEndpoint, error) {
	return w.Repo.ListForUser(ctx, userID)
}

// Create stores an enabled endpoint with a fresh secret.
func (w *Webhooks) Create(ctx context.Context, e domain.WebhookEndpoint) (domain.WebhookEndpoint, string, error) {
	secret, err := newWebhookSecret()
	if err != nil {
		return domain.WebhookEndpoint{}, "", ErrWebhookSecret
	}
	e.SigningSecret = secret
	e.Enabled = true
	created, err := w.Repo.Create(ctx, e)
	if err != nil {
		return domain.WebhookEndpoint{}, "", err
	}
	return created, secret, nil
}

// Update rewrites an endpoint's configuration.
func (w *Webhooks) Update(ctx context.Context, e domain.WebhookEndpoint) error {
	return w.Repo.Update(ctx, e)
}

// RotateSecret replaces the endpoint's secret and returns the new one.
func (w *Webhooks) RotateSecret(ctx context.Context, userID domain.UserID, id domain.WebhookEndpointID) (string, error) {
	secret, err := newWebhookSecret()
	if err != nil {
		return "", ErrWebhookSecret
	}
	if err := w.Repo.RotateSecret(ctx, id, userID, secret); err != nil {
		return "", err
	}
	return secret, nil
}

// Test sends a synthetic notification through the real dispatch path and
// returns the endpoint with its fresh delivery telemetry.
func (w *Webhooks) Test(ctx context.Context, userID domain.UserID, id domain.WebhookEndpointID) (domain.WebhookEndpoint, error) {
	if _, err := w.Repo.GetByID(ctx, id, userID); err != nil {
		return domain.WebhookEndpoint{}, err
	}
	nid, _ := uuid.NewV7()
	w.Deliver.Execute(ctx, []domain.Notification{{
		ID:           domain.NotificationID(nid),
		UserID:       userID,
		Type:         domain.NotificationTypeActivityImported,
		Severity:     domain.NotificationSeverityInfo,
		TitleI18nKey: "notif.test.title",
		BodyI18nKey:  "notif.test.body",
		CreatedAt:    w.now(),
	}})
	ep, _ := w.Repo.GetByID(ctx, id, userID)
	return ep, nil
}

// Delete removes one of the user's endpoints.
func (w *Webhooks) Delete(ctx context.Context, userID domain.UserID, id domain.WebhookEndpointID) error {
	return w.Repo.Delete(ctx, id, userID)
}
