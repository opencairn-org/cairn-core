package notification

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type settingsUsers struct {
	port.UserReader
	user domain.User
}

func (f *settingsUsers) GetUser(_ context.Context, id domain.UserID) (domain.User, error) {
	if id != f.user.ID {
		return domain.User{}, domain.ErrNotFound
	}
	return f.user, nil
}

type settingsMail struct {
	sent []port.EmailMessage
	err  error
}

func (f *settingsMail) Send(_ context.Context, m port.EmailMessage) error {
	f.sent = append(f.sent, m)
	return f.err
}

func actionKind(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func TestSettingsSendTestEmail(t *testing.T) {
	ctx := t.Context()
	alice := domain.User{ID: domain.UserID(uuid.New()), Email: "alice@example.com"}
	users := &settingsUsers{user: alice}
	mail := &settingsMail{}
	s := &Settings{Users: users, Mail: mail}

	if to, err := s.SendTestEmail(ctx, alice.ID); err != nil || to != alice.Email || len(mail.sent) != 1 {
		t.Fatalf("send = %q %v", to, err)
	}
	if _, err := s.SendTestEmail(ctx, domain.UserID(uuid.New())); actionKind(t, err) != port.ActionErrorInternal {
		t.Fatalf("unknown user = %v", err)
	}
	mail.err = errors.New("relay down")
	if _, err := s.SendTestEmail(ctx, alice.ID); actionKind(t, err) != port.ActionErrorUpstream || !strings.Contains(err.Error(), "relay down") {
		t.Fatalf("relay failure = %v", err)
	}
	users.user.Email = ""
	if _, err := s.SendTestEmail(ctx, alice.ID); actionKind(t, err) != port.ActionErrorInvalid {
		t.Fatalf("no address = %v", err)
	}
	s.Mail = nil
	if _, err := s.SendTestEmail(ctx, alice.ID); actionKind(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("no mail = %v", err)
	}
}

type crudWebhooks struct {
	port.WebhookEndpointRepo
	rows map[domain.WebhookEndpointID]domain.WebhookEndpoint
}

func (f *crudWebhooks) Create(_ context.Context, e domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	e.ID = domain.WebhookEndpointID(uuid.New())
	f.rows[e.ID] = e
	return e, nil
}
func (f *crudWebhooks) GetByID(_ context.Context, id domain.WebhookEndpointID, userID domain.UserID) (domain.WebhookEndpoint, error) {
	e, ok := f.rows[id]
	if !ok || e.UserID != userID {
		return domain.WebhookEndpoint{}, domain.ErrNotFound
	}
	return e, nil
}
func (f *crudWebhooks) RotateSecret(ctx context.Context, id domain.WebhookEndpointID, userID domain.UserID, secret string) error {
	e, err := f.GetByID(ctx, id, userID)
	if err != nil {
		return err
	}
	e.SigningSecret = secret
	f.rows[id] = e
	return nil
}

func TestWebhooksCreateRotateTest(t *testing.T) {
	ctx := t.Context()
	alice := domain.UserID(uuid.New())
	repo := &crudWebhooks{rows: map[domain.WebhookEndpointID]domain.WebhookEndpoint{}}
	w := &Webhooks{Repo: repo}

	created, secret, err := w.Create(ctx, domain.WebhookEndpoint{UserID: alice, Name: "hook", URL: "https://example.com"})
	if err != nil || !strings.HasPrefix(secret, "whsec_") || !created.Enabled || repo.rows[created.ID].SigningSecret != secret {
		t.Fatalf("create = %+v %q %v", created, secret, err)
	}
	next, err := w.RotateSecret(ctx, alice, created.ID)
	if err != nil || next == secret || repo.rows[created.ID].SigningSecret != next {
		t.Fatalf("rotate = %q %v", next, err)
	}
	if _, err := w.RotateSecret(ctx, domain.UserID(uuid.New()), created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rotate as stranger = %v", err)
	}
	// A nil dispatcher is a no-op; the endpoint comes back for its telemetry.
	if ep, err := w.Test(ctx, alice, created.ID); err != nil || ep.ID != created.ID {
		t.Fatalf("test = %+v %v", ep, err)
	}
	if _, err := w.Test(ctx, domain.UserID(uuid.New()), created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("test as stranger = %v", err)
	}
}
