package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Sessions opens and ends browser sessions; only the token's sha256 is stored.
type Sessions struct {
	Repo port.SessionRepo
	TTL  time.Duration
	Now  func() time.Time
}

// IssuedSession is a fresh session's plaintext token, shown to the client once.
type IssuedSession struct {
	Token     string
	ExpiresAt time.Time
}

func (s *Sessions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Issue creates a session for the user signed in by method.
func (s *Sessions) Issue(ctx context.Context, userID domain.UserID, method domain.SessionAuthMethod, userAgent string) (IssuedSession, error) {
	token, err := randomURLSafe(32)
	if err != nil {
		return IssuedSession{}, fmt.Errorf("session token: %w", err)
	}
	hash := sha256.Sum256([]byte(token))
	now := s.now()
	expiresAt := now.Add(s.TTL)
	if _, err := s.Repo.Create(ctx, domain.Session{
		UserID:           userID,
		TokenHash:        hash[:],
		AuthMethod:       method,
		UserAgentSummary: truncate(userAgent, 200),
		CreatedAt:        now,
		LastSeenAt:       now,
		ExpiresAt:        expiresAt,
	}); err != nil {
		return IssuedSession{}, fmt.Errorf("create session: %w", err)
	}
	return IssuedSession{Token: token, ExpiresAt: expiresAt}, nil
}

// Lookup is the id of the session behind token; zero when there is none.
func (s *Sessions) Lookup(ctx context.Context, token string) domain.SessionID {
	if token == "" {
		return domain.SessionID{}
	}
	hash := sha256.Sum256([]byte(token))
	sess, err := s.Repo.GetByTokenHash(ctx, hash[:])
	if err != nil {
		return domain.SessionID{}
	}
	return sess.ID
}

// End revokes the session behind token; best-effort, as logout always succeeds.
func (s *Sessions) End(ctx context.Context, token string) {
	if id := s.Lookup(ctx, token); id != (domain.SessionID{}) {
		_ = s.Repo.Revoke(ctx, id)
	}
}
