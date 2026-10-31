package notification

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeUserRepo struct {
	port.UserReader
	email string
}

func (f *fakeUserRepo) GetUser(_ context.Context, id domain.UserID) (domain.User, error) {
	return domain.User{ID: id, Email: f.email}, nil
}

type fakePrefRepo struct {
	rows []domain.NotificationPreference
}

func (f *fakePrefRepo) ListForUser(_ context.Context, _ domain.UserID) ([]domain.NotificationPreference, error) {
	return f.rows, nil
}
func (f *fakePrefRepo) Upsert(_ context.Context, _ domain.NotificationPreference) error { return nil }

type fakeEmail struct{ sent []port.EmailMessage }

func (f *fakeEmail) Send(_ context.Context, m port.EmailMessage) error {
	f.sent = append(f.sent, m)
	return nil
}

func deliverWith(email string, prefs []domain.NotificationPreference, e *fakeEmail) *DeliverNotifications {
	return NewDeliverNotifications(&fakeUserRepo{email: email}, nil, &fakePrefRepo{rows: prefs}, e, nil, nil, nil, "http://x", nil)
}

func notif(t domain.NotificationType, sev domain.NotificationSeverity) domain.Notification {
	return domain.Notification{UserID: domain.UserID(uuid.New()), Type: t, Severity: sev}
}

func TestDeliver_DefaultOnType_Emails(t *testing.T) {
	e := &fakeEmail{}
	deliverWith("a@b.c", nil, e).Execute(context.Background(),
		[]domain.Notification{notif(domain.NotificationTypeWorkerOffline, domain.NotificationSeverityWarn)})
	if len(e.sent) != 1 {
		t.Fatalf("expected 1 email (worker_offline default-on), got %d", len(e.sent))
	}
}

func TestDeliver_DefaultOffType_Skips(t *testing.T) {
	e := &fakeEmail{}
	deliverWith("a@b.c", nil, e).Execute(context.Background(),
		[]domain.Notification{notif(domain.NotificationTypeSegmentPersonalRecord, domain.NotificationSeverityInfo)})
	if len(e.sent) != 0 {
		t.Fatalf("expected 0 emails (PR default-off), got %d", len(e.sent))
	}
}

func TestDeliver_ExplicitOptIn_Emails(t *testing.T) {
	e := &fakeEmail{}
	n := notif(domain.NotificationTypeSegmentPersonalRecord, domain.NotificationSeverityInfo)
	prefs := []domain.NotificationPreference{{
		UserID: n.UserID, Type: domain.NotificationTypeSegmentPersonalRecord,
		Channel: domain.NotificationChannelEmail, Enabled: true, MinSeverity: domain.NotificationSeverityInfo,
	}}
	deliverWith("a@b.c", prefs, e).Execute(context.Background(), []domain.Notification{n})
	if len(e.sent) != 1 {
		t.Fatalf("expected 1 email after opt-in, got %d", len(e.sent))
	}
}

func TestDeliver_NoEmailAddress_Skips(t *testing.T) {
	e := &fakeEmail{}
	deliverWith("", nil, e).Execute(context.Background(),
		[]domain.Notification{notif(domain.NotificationTypeWorkerOffline, domain.NotificationSeverityWarn)})
	if len(e.sent) != 0 {
		t.Fatalf("expected 0 emails (no address), got %d", len(e.sent))
	}
}

func TestDeliver_NilEmailSender_NoPanic(t *testing.T) {
	var uc *DeliverNotifications
	uc.Execute(context.Background(), []domain.Notification{notif(1, "info")}) // nil receiver, must not panic
}
