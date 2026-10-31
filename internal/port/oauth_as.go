package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// OAuthClientStore registers and looks up OAuth clients.
type OAuthClientStore interface {
	CreateClient(ctx context.Context, c domain.OAuthClient) error
	GetClient(ctx context.Context, clientID string) (domain.OAuthClient, error)
}

// OAuthCodeStore issues and consumes authorization codes.
type OAuthCodeStore interface {
	CreateAuthCode(ctx context.Context, code domain.OAuthAuthorizationCode, codeHash []byte) error

	// ConsumeAuthCode atomically fetches and marks a code consumed. It returns
	// ErrNotFound if the code is missing, already consumed, or expired at now.
	ConsumeAuthCode(ctx context.Context, codeHash []byte, now time.Time) (domain.OAuthAuthorizationCode, error)
}

// OAuthTokenLookup resolves an access token by its hash.
type OAuthTokenLookup interface {
	FindAccessToken(ctx context.Context, tokenHash []byte) (domain.OAuthAccessToken, error)
}

// OAuthTokenStore issues, refreshes and revokes tokens.
type OAuthTokenStore interface {
	CreateAccessToken(ctx context.Context, t domain.OAuthAccessToken, tokenHash []byte) error
	RevokeAccessToken(ctx context.Context, tokenHash []byte) error
	CreateRefreshToken(ctx context.Context, t domain.OAuthRefreshToken, tokenHash []byte) error
	FindRefreshToken(ctx context.Context, tokenHash []byte) (domain.OAuthRefreshToken, error)
	RevokeRefreshToken(ctx context.Context, tokenHash []byte) error
}

// OAuthGrantStore lists and revokes a user's authorized apps.
type OAuthGrantStore interface {
	// ListGrantsForUser groups the user's live access and refresh tokens by
	// client (authorized apps), newest grant first.
	ListGrantsForUser(ctx context.Context, userID domain.UserID, now time.Time) ([]domain.OAuthGrant, error)

	// RevokeClientForUser revokes every live token the client holds for the
	// user and returns how many were revoked.
	RevokeClientForUser(ctx context.Context, userID domain.UserID, clientID string) (int, error)
}

// OAuthDeviceCodeStore backs the device authorization grant (RFC 8628).
type OAuthDeviceCodeStore interface {
	// CreateDeviceCode stores a pending request. It purges expired rows first
	// and returns ErrUnique when the user code collides with a live one.
	CreateDeviceCode(ctx context.Context, d domain.OAuthDeviceCode, deviceCodeHash []byte) error

	// FindDeviceCodeByUserCode returns the live request behind a canonical
	// user code; ErrNotFound when there is none.
	FindDeviceCodeByUserCode(ctx context.Context, userCode string, now time.Time) (domain.OAuthDeviceCode, error)

	// DecideDeviceCode records the user's decision on a pending, live request;
	// ErrNotFound when the code is unknown, expired or already decided.
	DecideDeviceCode(ctx context.Context, userCode string, userID domain.UserID, approve bool, grantedScope string, now time.Time) error

	// PollDeviceCode returns the request behind a device code hash with
	// LastPolledAt as it was before this poll, and records now as the poll
	// time. ErrNotFound when unknown or consumed; expiry is the caller's call.
	PollDeviceCode(ctx context.Context, deviceCodeHash []byte, now time.Time) (domain.OAuthDeviceCode, error)

	// ConsumeDeviceCode marks an approved, live request consumed so its
	// approval issues tokens once; ErrNotFound otherwise.
	ConsumeDeviceCode(ctx context.Context, deviceCodeHash []byte, now time.Time) (domain.OAuthDeviceCode, error)
}

// OAuthServerRepo persists the OAuth 2.1 authorization-server state: registered
// clients, one-time authorization codes, and the access/refresh tokens issued.
// Only token/secret hashes are stored. Returns domain.ErrNotFound on misses.
type OAuthServerRepo interface {
	OAuthClientStore
	OAuthCodeStore
	OAuthTokenLookup
	OAuthTokenStore
	OAuthGrantStore
	OAuthDeviceCodeStore
}
