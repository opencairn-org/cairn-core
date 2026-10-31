package connect

import (
	"context"
	"strings"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
)

type fakeChallengeRepo struct {
	port.LoginChallengeRepo
	user      domain.UserID
	satisfied bool
	consumed  bool
}

func (f *fakeChallengeRepo) GetByTokenHash(context.Context, []byte) (domain.LoginChallenge, error) {
	return domain.LoginChallenge{UserID: f.user, Purpose: domain.ChallengeSecondFactor}, nil
}

func (f *fakeChallengeRepo) Consume(_ context.Context, _ []byte, _ time.Time) (domain.UserID, error) {
	if !f.satisfied || f.consumed {
		return domain.UserID{}, port.ErrAuthTokenInvalid
	}
	f.consumed = true
	return f.user, nil
}

type creatingSessions struct {
	fakeSessions
	created []domain.Session
}

func (c *creatingSessions) Create(_ context.Context, s domain.Session) (domain.SessionID, error) {
	c.created = append(c.created, s)
	return domain.SessionID(uuid.New()), nil
}

type sfUserRepo struct {
	port.UserReader
	port.UserProfileWriter
	port.UserCredentialStore
	user    domain.User
	enabled bool
}

func (u *sfUserRepo) GetUser(context.Context, domain.UserID) (domain.User, error) {
	u.user.PasskeySecondFactor = u.enabled
	return u.user, nil
}
func (u *sfUserRepo) SetPasskeySecondFactor(_ context.Context, _ domain.UserID, enabled bool) error {
	u.enabled = enabled
	return nil
}

type sfPasskeyRepo struct {
	port.PasskeyRepo
	n int
}

func (p *sfPasskeyRepo) ListByUser(context.Context, domain.UserID) ([]domain.Passkey, error) {
	return make([]domain.Passkey, p.n), nil
}

func TestComplete2FAAndToggle(t *testing.T) {
	me := domain.UserID(uuid.New())
	challenges := &fakeChallengeRepo{user: me}
	sessions := &creatingSessions{}
	users := &sfUserRepo{user: domain.User{ID: me, Username: "alice", Status: domain.UserStatusActive}}
	keys := &sfPasskeyRepo{}
	s := &AuthServer{Users: users, Sessions: sessions, SessionTTL: time.Hour,
		SecondFactor: &authuc.SecondFactor{Challenges: challenges, Users: users, Passkeys: keys}}

	if _, err := (&AuthServer{}).Complete2FA(t.Context(), &v1.Complete2FARequest{ChallengeToken: "x"}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("unwired: %v", err)
	}
	if _, err := s.Complete2FA(t.Context(), &v1.Complete2FARequest{ChallengeToken: "x"}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("unsatisfied: %v", err)
	}
	challenges.satisfied = true
	res, err := s.Complete2FA(t.Context(), &v1.Complete2FARequest{ChallengeToken: "x", Method: v1.TwoFactorMethod_TWO_FACTOR_METHOD_WEBAUTHN})
	if err != nil {
		t.Fatal(err)
	}
	sess := res.GetSession()
	if !strings.HasPrefix(sess.GetSessionToken(), domain.SessionTokenPrefix) || sess.GetUser().GetUsername() != "alice" || len(sessions.created) != 1 {
		t.Fatalf("session %v created %d", sess, len(sessions.created))
	}
	if sessions.created[0].AuthMethod != domain.SessionAuthPasswordWithWebauthn || sessions.created[0].ExpiresAt.Sub(sessions.created[0].CreatedAt) != time.Hour {
		t.Fatalf("session row %+v", sessions.created[0])
	}
	if _, err := s.Complete2FA(t.Context(), &v1.Complete2FARequest{ChallengeToken: "x"}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("replay: %v", err)
	}

	ctx := context.WithValue(t.Context(), ctxKeyUserID, me)
	if _, err := s.SetPasskeySecondFactor(ctx, &v1.SetPasskeySecondFactorRequest{Enabled: true}); connectrpc.CodeOf(err) != connectrpc.CodeFailedPrecondition {
		t.Fatalf("enable without passkey: %v", err)
	}
	keys.n = 1
	on, err := s.SetPasskeySecondFactor(ctx, &v1.SetPasskeySecondFactorRequest{Enabled: true})
	if err != nil || !on.GetUser().GetPasskeySecondFactor() {
		t.Fatalf("enable: %v %v", on, err)
	}
	off, err := s.SetPasskeySecondFactor(ctx, &v1.SetPasskeySecondFactorRequest{Enabled: false})
	if err != nil || off.GetUser().GetPasskeySecondFactor() {
		t.Fatalf("disable: %v %v", off, err)
	}
}
