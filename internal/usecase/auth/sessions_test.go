package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type issueSessions struct {
	port.SessionRepo
	rows    map[string]domain.Session
	revoked []domain.SessionID
}

func (f *issueSessions) Create(_ context.Context, s domain.Session) (domain.SessionID, error) {
	s.ID = domain.SessionID(uuid.New())
	f.rows[string(s.TokenHash)] = s
	return s.ID, nil
}
func (f *issueSessions) GetByTokenHash(_ context.Context, hash []byte) (domain.Session, error) {
	if s, ok := f.rows[string(hash)]; ok {
		return s, nil
	}
	return domain.Session{}, domain.ErrNotFound
}
func (f *issueSessions) Revoke(_ context.Context, id domain.SessionID) error {
	f.revoked = append(f.revoked, id)
	return nil
}

func TestSessionsIssueLookupEnd(t *testing.T) {
	ctx := t.Context()
	repo := &issueSessions{rows: map[string]domain.Session{}}
	now := time.Unix(1000, 0)
	s := &Sessions{Repo: repo, TTL: time.Hour, Now: func() time.Time { return now }}
	alice := domain.UserID(uuid.New())

	got, err := s.Issue(ctx, alice, domain.SessionAuthPassword, strings.Repeat("a", 300))
	if err != nil || got.Token == "" || !got.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("issue = %+v, %v", got, err)
	}
	hash := sha256.Sum256([]byte(got.Token))
	row, ok := repo.rows[string(hash[:])]
	if !ok || row.UserID != alice || row.AuthMethod != domain.SessionAuthPassword || len(row.UserAgentSummary) != 200 {
		t.Fatalf("stored = %+v", row)
	}
	if id := s.Lookup(ctx, got.Token); id != row.ID {
		t.Fatalf("lookup = %v", id)
	}
	if id := s.Lookup(ctx, "other"); id != (domain.SessionID{}) {
		t.Fatalf("unknown token resolved to %v", id)
	}
	s.End(ctx, "other")
	s.End(ctx, got.Token)
	if len(repo.revoked) != 1 || repo.revoked[0] != row.ID {
		t.Fatalf("revoked = %v", repo.revoked)
	}
}

type loginUsers struct {
	port.UserCredentialStore
	user domain.User
	hash string
	err  error
}

func (f *loginUsers) GetCredentialsByLogin(_ context.Context, login string) (domain.User, domain.Credentials, error) {
	if f.err != nil {
		return domain.User{}, domain.Credentials{}, f.err
	}
	if login != f.user.Username {
		return domain.User{}, domain.Credentials{}, domain.ErrNotFound
	}
	return f.user, domain.Credentials{UserID: f.user.ID, PasswordHash: f.hash}, nil
}

type verifyHasher struct{ calls int }

func (h *verifyHasher) Verify(pw, enc string) (bool, error) {
	h.calls++
	return enc == "h:"+pw, nil
}

func TestPasswordLogin(t *testing.T) {
	ctx := t.Context()
	alice := domain.User{ID: domain.UserID(uuid.New()), Username: "alice", Status: domain.UserStatusActive}
	users := &loginUsers{user: alice, hash: "h:secret"}
	hasher := &verifyHasher{}
	p := &PasswordLogin{Users: users, Hasher: hasher, Guard: &LoginGuard{MaxPerUser: 2}}

	if u, err := p.Login(ctx, "alice", "secret", "1.2.3.4"); err != nil || u.ID != alice.ID {
		t.Fatalf("good login = %v, %v", u.ID, err)
	}
	// An unknown user still costs a hash round.
	if _, err := p.Login(ctx, "nobody", "x", "1.2.3.4"); !errors.Is(err, ErrInvalidCredentials) || hasher.calls != 2 {
		t.Fatalf("unknown user = %v (verify calls %d)", err, hasher.calls)
	}
	for range 2 {
		if _, err := p.Login(ctx, "alice", "wrong", "1.2.3.4"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("wrong password = %v", err)
		}
	}
	if _, err := p.Login(ctx, "alice", "secret", "1.2.3.4"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("locked = %v", err)
	}

	users.user.Status = domain.UserStatusSuspended
	p.Guard = nil
	if _, err := p.Login(ctx, "alice", "secret", "1.2.3.4"); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("suspended = %v", err)
	}
	users.err = errors.New("db down")
	if _, err := p.Login(ctx, "alice", "secret", "1.2.3.4"); err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("lookup failure = %v", err)
	}
}
