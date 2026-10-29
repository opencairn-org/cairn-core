package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// OAuthServerRepo persists the OAuth 2.1 authorization-server state: registered
// clients, one-time authorization codes, and the access/refresh tokens issued.
// Only token/secret hashes are stored. Returns domain.ErrNotFound on misses.
type OAuthServerRepo interface {
	// ---- clients ----
	CreateClient(ctx context.Context, c domain.OAuthClient) error
	GetClient(ctx context.Context, clientID string) (domain.OAuthClient, error)

	// ---- authorization codes ----
	CreateAuthCode(ctx context.Context, code domain.OAuthAuthorizationCode, codeHash []byte) error
	// ConsumeAuthCode atomically fetches and marks a code consumed. It returns
	// ErrNotFound if the code is missing, already consumed, or expired at now.
	ConsumeAuthCode(ctx context.Context, codeHash []byte, now time.Time) (domain.OAuthAuthorizationCode, error)

	// ---- access tokens ----
	CreateAccessToken(ctx context.Context, t domain.OAuthAccessToken, tokenHash []byte) error
	FindAccessToken(ctx context.Context, tokenHash []byte) (domain.OAuthAccessToken, error)
	RevokeAccessToken(ctx context.Context, tokenHash []byte) error

	// ---- refresh tokens ----
	CreateRefreshToken(ctx context.Context, t domain.OAuthRefreshToken, tokenHash []byte) error
	FindRefreshToken(ctx context.Context, tokenHash []byte) (domain.OAuthRefreshToken, error)
	RevokeRefreshToken(ctx context.Context, tokenHash []byte) error

	// ListGrantsForUser groups the user's live access and refresh tokens by
	// client (authorized apps), newest grant first.
	ListGrantsForUser(ctx context.Context, userID domain.UserID, now time.Time) ([]domain.OAuthGrant, error)
	// RevokeClientForUser revokes every live token the client holds for the
	// user and returns how many were revoked.
	RevokeClientForUser(ctx context.Context, userID domain.UserID, clientID string) (int, error)
}
