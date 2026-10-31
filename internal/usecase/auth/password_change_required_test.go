package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type pcUsers struct {
	port.UserLoginStore
	rows   map[domain.UserID]*domain.User
	hashes map[domain.UserID]string
}

func (u *pcUsers) GetUser(_ context.Context, id domain.UserID) (domain.User, error) {
	if row, ok := u.rows[id]; ok {
		return *row, nil
	}
	return domain.User{}, domain.ErrNotFound
}

func (u *pcUsers) UpdatePassword(_ context.Context, id domain.UserID, encoded string) error {
	u.hashes[id] = encoded
	u.rows[id].MustChangePassword = false
	return nil
}

type plainHasher struct{}

func (plainHasher) Hash(p string) (string, error) { return "h:" + p, nil }

func TestPasswordChangeRequired(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	ctx := t.Context()
	alice := domain.UserID(uuid.New())
	users := &pcUsers{rows: map[domain.UserID]*domain.User{alice: {ID: alice, MustChangePassword: true}}, hashes: map[domain.UserID]string{}}
	challenges := &fakeChallenges{rows: map[string]*domain.LoginChallenge{}}
	pc := &PasswordChangeRequired{Challenges: challenges, Users: users, Hasher: plainHasher{}, Now: func() time.Time { return now }}
	sf := &SecondFactor{Challenges: challenges, Users: &sfUsers{enabled: map[domain.UserID]bool{}}, Passkeys: &sfPasskeys{}, Now: func() time.Time { return now }}

	if !pc.Required(domain.User{MustChangePassword: true}) || pc.Required(domain.User{}) || (*PasswordChangeRequired)(nil).Required(domain.User{MustChangePassword: true}) {
		t.Fatal("Required")
	}
	token, err := pc.Begin(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	// The password-change token is no second factor and vice versa.
	if err := sf.Satisfy(ctx, token, alice); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("satisfy with password token: %v", err)
	}
	if _, err := sf.Complete(ctx, token); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("complete with password token: %v", err)
	}
	sfToken, _ := sf.Begin(ctx, alice)
	if _, err := pc.Complete(ctx, sfToken, "a strong enough password"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("complete with 2fa token: %v", err)
	}
	if _, err := pc.Complete(ctx, token, "short"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("weak password: %v", err)
	}
	user, err := pc.Complete(ctx, token, "a strong enough password")
	if err != nil || user.ID != alice || user.MustChangePassword || users.hashes[alice] != "h:a strong enough password" {
		t.Fatalf("complete: %v %+v %v", err, user, users.hashes)
	}
	if _, err := pc.Complete(ctx, token, "a strong enough password"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("token reuse: %v", err)
	}
	// Flag already cleared elsewhere: the challenge is refused.
	again, _ := pc.Begin(ctx, alice)
	if _, err := pc.Complete(ctx, again, "another strong password"); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("no longer required: %v", err)
	}
	// Expired challenge.
	users.rows[alice].MustChangePassword = true
	stale, _ := pc.Begin(ctx, alice)
	pc.Now = func() time.Time { return now.Add(domain.LoginChallengeTTL + time.Second) }
	if _, err := pc.Complete(ctx, stale, "another strong password"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("expired: %v", err)
	}
}
