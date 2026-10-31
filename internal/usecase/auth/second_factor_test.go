package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeChallenges struct {
	rows map[string]*domain.LoginChallenge
}

func (f *fakeChallenges) Create(_ context.Context, c domain.LoginChallenge) (domain.LoginChallengeID, error) {
	c.ID = domain.LoginChallengeID(uuid.New())
	f.rows[string(c.TokenHash)] = &c
	return c.ID, nil
}
func (f *fakeChallenges) GetByTokenHash(_ context.Context, hash []byte) (domain.LoginChallenge, error) {
	if c, ok := f.rows[string(hash)]; ok {
		return *c, nil
	}
	return domain.LoginChallenge{}, domain.ErrNotFound
}
func (f *fakeChallenges) MarkSatisfied(_ context.Context, id domain.LoginChallengeID, now time.Time) error {
	for _, c := range f.rows {
		if c.ID == id && c.SatisfiedAt == nil && c.ConsumedAt == nil && now.Before(c.ExpiresAt) {
			c.SatisfiedAt = &now
			return nil
		}
	}
	return port.ErrAuthTokenInvalid
}
func (f *fakeChallenges) Consume(_ context.Context, hash []byte, now time.Time) (domain.UserID, error) {
	c, ok := f.rows[string(hash)]
	if !ok || !c.Completable(now) {
		return domain.UserID{}, port.ErrAuthTokenInvalid
	}
	c.ConsumedAt = &now
	return c.UserID, nil
}

type sfUsers struct {
	port.UserCredentialStore
	enabled map[domain.UserID]bool
}

func (u *sfUsers) SetPasskeySecondFactor(_ context.Context, id domain.UserID, enabled bool) error {
	u.enabled[id] = enabled
	return nil
}

type sfPasskeys struct {
	port.PasskeyRepo
	keys []domain.Passkey
}

func (p *sfPasskeys) ListByUser(context.Context, domain.UserID) ([]domain.Passkey, error) {
	return p.keys, nil
}

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func TestSecondFactorFlow(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	keys := &sfPasskeys{}
	users := &sfUsers{enabled: map[domain.UserID]bool{}}
	sf := &SecondFactor{Challenges: &fakeChallenges{rows: map[string]*domain.LoginChallenge{}}, Users: users, Passkeys: keys, Now: func() time.Time { return now }}
	me, other := domain.UserID(uuid.New()), domain.UserID(uuid.New())

	if err := sf.SetEnabled(t.Context(), me, true); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("enable without passkey: %v", err)
	}
	keys.keys = []domain.Passkey{{UserID: me}}
	if err := sf.SetEnabled(t.Context(), me, true); err != nil || !users.enabled[me] {
		t.Fatalf("enable: %v", err)
	}
	if !sf.Required(domain.User{PasskeySecondFactor: true}) || sf.Required(domain.User{}) {
		t.Fatal("required")
	}

	token, err := sf.Begin(t.Context(), me)
	if err != nil || token == "" {
		t.Fatalf("begin: %v", err)
	}
	// Not satisfied yet: cannot complete.
	if _, err := sf.Complete(t.Context(), token); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("premature complete: %v", err)
	}
	// Another user's passkey cannot satisfy it.
	if err := sf.Satisfy(t.Context(), token, other); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("foreign satisfy: %v", err)
	}
	if err := sf.Satisfy(t.Context(), token, me); err != nil {
		t.Fatalf("satisfy: %v", err)
	}
	uid, err := sf.Complete(t.Context(), token)
	if err != nil || uid != me {
		t.Fatalf("complete: %v %s", err, uid)
	}
	// One-time.
	if _, err := sf.Complete(t.Context(), token); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("replay: %v", err)
	}
	// Expiry.
	token, _ = sf.Begin(t.Context(), me)
	_ = sf.Satisfy(t.Context(), token, me)
	sf.Now = func() time.Time { return now.Add(10 * time.Minute) }
	if _, err := sf.Complete(t.Context(), token); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("expired: %v", err)
	}
	if err := sf.SetEnabled(t.Context(), me, false); err != nil || users.enabled[me] {
		t.Fatalf("disable: %v", err)
	}
}
