package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type verifyTokens struct {
	port.AuthTokenRepo
	byHash map[string]domain.UserID
}

func (f *verifyTokens) CreateEmailVerificationToken(_ context.Context, uid domain.UserID, _ string, hash []byte, _ time.Time) error {
	f.byHash[string(hash)] = uid
	return nil
}
func (f *verifyTokens) ConsumeEmailVerificationToken(_ context.Context, hash []byte, _ time.Time) (domain.UserID, string, error) {
	uid, ok := f.byHash[string(hash)]
	if !ok {
		return domain.UserID{}, "", port.ErrAuthTokenInvalid
	}
	delete(f.byHash, string(hash))
	return uid, "", nil
}

type verifyUsers struct {
	fakeUsers
	verified []domain.UserID
}

func (f *verifyUsers) UpdateEmailVerified(_ context.Context, id domain.UserID, _ bool) error {
	f.verified = append(f.verified, id)
	return nil
}

func TestEmailVerification(t *testing.T) {
	ctx := t.Context()
	alice := domain.User{ID: domain.UserID(uuid.New()), Email: "alice@example.com"}
	users := &verifyUsers{fakeUsers: fakeUsers{user: alice}}
	tokens := &verifyTokens{byHash: map[string]domain.UserID{}}
	mail := &fakeMail{}
	e := &EmailVerification{Users: users, Tokens: tokens, Mail: mail, Link: func(c string) string { return "https://x/verify?code=" + c }}

	already, err := e.Resend(ctx, alice.ID)
	if err != nil || already || len(mail.sent) != 1 || mail.sent[0].To != alice.Email {
		t.Fatalf("resend = %v %v %+v", already, err, mail.sent)
	}
	_, code, _ := strings.Cut(mail.sent[0].TextBody, "code=")
	code, _, _ = strings.Cut(code, "\n")
	if err := e.Confirm(ctx, code); err != nil || len(users.verified) != 1 || users.verified[0] != alice.ID {
		t.Fatalf("confirm = %v %v", err, users.verified)
	}
	if err := e.Confirm(ctx, code); !errors.Is(err, port.ErrAuthTokenInvalid) {
		t.Fatalf("second confirm = %v", err)
	}

	users.user.EmailVerified = true
	if already, err := e.Resend(ctx, alice.ID); err != nil || !already {
		t.Fatalf("verified resend = %v %v", already, err)
	}
	if _, err := e.Resend(ctx, domain.UserID(uuid.New())); kindOf(t, err) != port.ActionErrorInternal {
		t.Fatalf("unknown user = %v", err)
	}
	users.user.EmailVerified = false
	e.Mail = nil
	if _, err := e.Resend(ctx, alice.ID); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("no mail = %v", err)
	}
	if err := e.Send(ctx, alice); !errors.Is(err, ErrEmailUnavailable) {
		t.Fatalf("send without mail = %v", err)
	}
}

type ownPasskeys struct {
	fakePasskeys
	created []domain.Passkey
	updated [][]byte
}

func (f *ownPasskeys) Create(_ context.Context, p domain.Passkey) (domain.PasskeyID, error) {
	f.created = append(f.created, p)
	return domain.PasskeyID(uuid.New()), nil
}
func (f *ownPasskeys) UpdateCredential(_ context.Context, _, credJSON []byte) error {
	f.updated = append(f.updated, credJSON)
	return nil
}
func (f *ownPasskeys) GetByCredentialID(_ context.Context, id []byte) (domain.Passkey, error) {
	for _, k := range f.keys {
		if string(k.CredentialID) == string(id) {
			return k, nil
		}
	}
	return domain.Passkey{}, domain.ErrNotFound
}

func TestPasskeys(t *testing.T) {
	ctx := t.Context()
	alice := domain.User{ID: domain.UserID(uuid.New()), PasskeySecondFactor: true}
	repo := &ownPasskeys{fakePasskeys: fakePasskeys{keys: []domain.Passkey{{UserID: alice.ID, CredentialID: []byte("c1")}}}}
	p := &Passkeys{Repo: repo, Users: &fakeUsers{user: alice}}

	u, keys, err := p.Holder(ctx, alice.ID)
	if err != nil || u.ID != alice.ID || len(keys) != 1 {
		t.Fatalf("holder = %v %v %v", u.ID, keys, err)
	}
	if _, _, err := p.Holder(ctx, domain.UserID(uuid.New())); err == nil {
		t.Fatal("holder of unknown user")
	}
	if err := p.Register(ctx, alice.ID, []byte("c2"), []byte("{}"), "Laptop"); err != nil || len(repo.created) != 1 || repo.created[0].Name != "Laptop" {
		t.Fatalf("register = %v %+v", err, repo.created)
	}
	if pk, err := p.Used(ctx, []byte("c1"), []byte(`{"n":2}`)); err != nil || pk.UserID != alice.ID || len(repo.updated) != 1 {
		t.Fatalf("used = %+v %v", pk, err)
	}
	if _, err := p.Used(ctx, []byte("nope"), nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("used unknown = %v", err)
	}
	if keys, second, err := p.List(ctx, alice.ID); err != nil || len(keys) != 1 || !second {
		t.Fatalf("list = %v %v %v", keys, second, err)
	}
}
