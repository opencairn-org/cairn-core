package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// SessionRepo persists server-side sessions for password / OIDC /
// passkey / PAT auth.
//
// Tokens are never stored in plaintext — callers pass the SHA-256 of
// the bearer token. The repo writes/reads the hash exclusively.
//
// Lookups by hash return domain.ErrNotFound when no row matches.
// Expired or revoked sessions are returned as-is; the caller must
// verify Session.IsActive(now) before treating the bearer as valid.
type SessionRepo interface {
	// Create inserts a fresh session row. The caller has already hashed
	// the plaintext token. The returned ID is server-allocated.
	Create(ctx context.Context, s domain.Session) (domain.SessionID, error)

	// GetByTokenHash returns the session row whose token_hash equals
	// the supplied hash. The caller checks IsActive(now) before trusting
	// it.
	GetByTokenHash(ctx context.Context, hash []byte) (domain.Session, error)

	// TouchLastSeen updates last_seen_at. Best-effort: the caller
	// should not fail the request when this errors.
	TouchLastSeen(ctx context.Context, id domain.SessionID) error

	// Revoke marks one session revoked. Idempotent for already-revoked
	// sessions.
	Revoke(ctx context.Context, id domain.SessionID) error

	// RevokeAllForUser revokes every active session for the user. Used
	// during full-logout and on credential changes (password reset,
	// passkey rotation).
	RevokeAllForUser(ctx context.Context, userID domain.UserID) error

	// ListForUser returns the user's active sessions (not revoked, not
	// expired), most recently seen first. Token hashes are left empty.
	ListForUser(ctx context.Context, userID domain.UserID) ([]domain.Session, error)
}
