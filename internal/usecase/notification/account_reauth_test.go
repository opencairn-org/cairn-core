package notification

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeAccountRepo struct {
	port.ExternalAccountRepo
	acct domain.ExternalAccount
	err  error
}

func (f *fakeAccountRepo) GetExternalAccount(_ context.Context, _ domain.ExternalAccountID) (domain.ExternalAccount, error) {
	return f.acct, f.err
}

type fakeNotifRepo struct {
	port.NotificationRepo
	saved []domain.Notification
}

func (f *fakeNotifRepo) SaveNotifications(_ context.Context, n []domain.Notification) error {
	f.saved = append(f.saved, n...)
	return nil
}

type fakeTx struct{}

func (fakeTx) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

func TestNotifyAccountNeedsReauth_CreatesAndDelivers(t *testing.T) {
	uid := domain.UserID(uuid.New())
	aid := domain.ExternalAccountID(uuid.New())
	accounts := &fakeAccountRepo{acct: domain.ExternalAccount{
		ID: aid, UserID: uid, Provider: "strava", DisplayLabel: "My Strava",
	}}
	notifs := &fakeNotifRepo{}
	email := &fakeEmail{}
	// deliver with email so we can assert fan-out happened (error severity
	// defaults email-on for this type).
	deliver := NewDeliverNotifications(&fakeUserRepo{email: "a@b.c"}, nil, &fakePrefRepo{}, email, nil, nil, nil, nil, "http://x", nil)

	uc := NewNotifyAccountNeedsReauth(accounts, notifs, fakeTx{}, deliver, nil, nil)
	if err := uc.Execute(context.Background(), aid, "invalid_grant"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(notifs.saved) != 1 {
		t.Fatalf("expected 1 saved notification, got %d", len(notifs.saved))
	}
	n := notifs.saved[0]
	if n.UserID != uid {
		t.Fatalf("notification user = %s, want %s", n.UserID, uid)
	}
	if n.Type != domain.NotificationTypeExternalAccountRefreshFailed {
		t.Fatalf("type = %v", n.Type)
	}
	if n.Severity != domain.NotificationSeverityError {
		t.Fatalf("severity = %v, want error", n.Severity)
	}
	if n.ExternalAccountID == nil || *n.ExternalAccountID != aid {
		t.Fatal("ExternalAccountID ref missing")
	}
	if n.DedupKey != "account_reauth:"+aid.String() {
		t.Fatalf("dedup key = %q", n.DedupKey)
	}
	if n.I18nParams["provider"] != "strava" || n.I18nParams["label"] != "My Strava" {
		t.Fatalf("i18n params = %v", n.I18nParams)
	}
	if len(email.sent) != 1 {
		t.Fatalf("expected 1 email (error severity default-on), got %d", len(email.sent))
	}
}

func TestNotifyAccountNeedsReauth_AccountLookupError(t *testing.T) {
	accounts := &fakeAccountRepo{err: errors.New("gone")}
	uc := NewNotifyAccountNeedsReauth(accounts, &fakeNotifRepo{}, fakeTx{}, nil, nil, nil)
	if err := uc.Execute(context.Background(), domain.ExternalAccountID(uuid.New()), "x"); err == nil {
		t.Fatal("expected error when account lookup fails")
	}
}
