package auth

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type callerSessions struct {
	port.SessionRepo
	rows    map[string]domain.Session
	touched []domain.SessionID
}

func (f *callerSessions) GetByTokenHash(_ context.Context, hash []byte) (domain.Session, error) {
	if s, ok := f.rows[string(hash)]; ok {
		return s, nil
	}
	return domain.Session{}, domain.ErrNotFound
}

func (f *callerSessions) TouchLastSeen(_ context.Context, id domain.SessionID) error {
	f.touched = append(f.touched, id)
	return nil
}

type callerPATs struct {
	port.PATRepo
	rows    map[string]domain.PersonalAccessToken
	touched int
}

func (f *callerPATs) FindByTokenHash(_ context.Context, hash []byte) (domain.PersonalAccessToken, error) {
	if p, ok := f.rows[string(hash)]; ok {
		return p, nil
	}
	return domain.PersonalAccessToken{}, domain.ErrNotFound
}

func (f *callerPATs) TouchLastUsed(context.Context, domain.PATID) error {
	f.touched++
	return nil
}

type callerOAuth map[string]domain.OAuthAccessToken

func (f callerOAuth) FindAccessToken(_ context.Context, hash []byte) (domain.OAuthAccessToken, error) {
	if t, ok := f[string(hash)]; ok {
		return t, nil
	}
	return domain.OAuthAccessToken{}, domain.ErrNotFound
}

func hashKey(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return string(h[:])
}

func TestCallersAuthenticate(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(1000, 0)
	alice := domain.UserID(uuid.New())
	sid := domain.SessionID(uuid.New())
	const (
		cookie   = "cookie-token"
		bearer   = domain.SessionTokenPrefix + "x"
		pat      = domain.PATTokenPrefix + "x"
		readTok  = domain.OAuthAccessTokenPrefix + "read"
		emptyTok = domain.OAuthAccessTokenPrefix + "empty"
	)
	sess := domain.Session{ID: sid, UserID: alice, ExpiresAt: now.Add(time.Hour)}
	sessions := &callerSessions{rows: map[string]domain.Session{hashKey(cookie): sess, hashKey(bearer): sess}}
	pats := &callerPATs{rows: map[string]domain.PersonalAccessToken{hashKey(pat): {UserID: alice}}}
	oauth := callerOAuth{
		hashKey(readTok):  {UserID: alice, Scope: "activities:read", ExpiresAt: now.Add(time.Hour)},
		hashKey(emptyTok): {UserID: alice, ExpiresAt: now.Add(time.Hour)},
	}
	c := &Callers{Sessions: sessions, PATs: pats, OAuth: oauth, Now: func() time.Time { return now }}

	got, ok := c.Authenticate(ctx, Credential{Cookie: cookie})
	if !ok || got.Kind != CallerSession || got.SessionID != sid || got.UserID != alice || len(sessions.touched) != 0 {
		t.Fatalf("cookie = %+v %v, touched %v", got, ok, sessions.touched)
	}
	// A stale cookie falls through to the bearer; Touch records last-seen.
	got, ok = c.Authenticate(ctx, Credential{Cookie: "stale", Bearer: bearer, TouchSession: true})
	if !ok || got.Kind != CallerSession || got.SessionID != sid || len(sessions.touched) != 1 {
		t.Fatalf("bearer session = %+v %v, touched %v", got, ok, sessions.touched)
	}
	got, ok = c.Authenticate(ctx, Credential{Bearer: pat})
	if !ok || got.Kind != CallerPAT || got.Scopes != nil || pats.touched != 1 {
		t.Fatalf("pat = %+v %v", got, ok)
	}
	got, ok = c.Authenticate(ctx, Credential{Bearer: readTok})
	if !ok || got.Kind != CallerOAuth || len(got.Scopes) != 1 || got.Scopes[0] != "activities:read" {
		t.Fatalf("oauth = %+v %v", got, ok)
	}
	if got, ok = c.Authenticate(ctx, Credential{Bearer: emptyTok}); !ok || got.Scopes == nil {
		t.Fatalf("scopeless oauth must carry non-nil scopes: %+v %v", got, ok)
	}
	if _, ok = c.Authenticate(ctx, Credential{Bearer: readTok, Mutating: true}); ok {
		t.Fatal("read-only token admitted to a mutation")
	}
	if _, ok = c.Authenticate(ctx, Credential{Bearer: readTok, NoOAuth: true}); ok {
		t.Fatal("oauth token admitted with NoOAuth")
	}
	if _, ok = c.Authenticate(ctx, Credential{Bearer: "garbage"}); ok {
		t.Fatal("unknown bearer resolved")
	}
	if uid, ok := c.Resolve(ctx, Credential{Bearer: pat}); !ok || uid != alice {
		t.Fatalf("resolve = %v %v", uid, ok)
	}
}
