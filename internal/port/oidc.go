package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// OIDC providers are configured via env (CAIRN_OIDC_*) and held in memory —
// there is no client repository. The only persisted OIDC state is the
// user↔provider link below.

// OIDCAuthRequest is the input to an authorization URL. CodeChallenge is the
// S256 PKCE challenge, empty without PKCE.
type OIDCAuthRequest struct {
	RedirectURL   string
	State         string
	Nonce         string
	CodeChallenge string
}

// OIDCClient speaks the authorization-code flow with an IdP (discovery,
// token endpoint, id_token signature/issuer/audience checks). Nonce and
// subject policy stay with the caller.
type OIDCClient interface {
	AuthCodeURL(ctx context.Context, p domain.OIDCProvider, req OIDCAuthRequest) (string, error)
	// Exchange redeems code (codeVerifier empty without PKCE) and verifies the
	// id_token. Errors wrap ErrOIDCCodeExchange or ErrOIDCIDTokenInvalid; any
	// other error is a discovery failure.
	Exchange(ctx context.Context, p domain.OIDCProvider, redirectURL, code, codeVerifier string) (domain.OIDCIDToken, error)
}

// OIDCClient error classes.
var (
	ErrOIDCCodeExchange   = repoError("oauth code exchange failed")
	ErrOIDCIDTokenInvalid = repoError("id_token verification failed")
)

// LinkedIdentityRepo persists the (provider, subject) → user_id mapping that
// closes the OIDC loop. `provider` is the OIDCProvider.ID string. Multiple
// identities can attach to one user.
type LinkedIdentityRepo interface {
	// FindBySubject looks up an existing link. ErrNotFound when no row matches
	// (i.e. first-time login for this subject on this provider).
	FindBySubject(ctx context.Context, provider, subject string) (domain.LinkedIdentity, error)

	// Create attaches a (provider, subject) to a user. The pair is unique —
	// duplicate inserts return an error wrapping domain.ErrUnique.
	Create(ctx context.Context, li domain.LinkedIdentity) (domain.LinkedIdentityID, error)

	// TouchLastUsed updates last_used_at + last_claims. Best-effort.
	TouchLastUsed(ctx context.Context, id domain.LinkedIdentityID, claims map[string]any) error

	// ListForUser returns the user's links, oldest first.
	ListForUser(ctx context.Context, userID domain.UserID) ([]domain.LinkedIdentity, error)

	// Delete removes one link; ownership-scoped. ErrNotFound when no row matches.
	Delete(ctx context.Context, userID domain.UserID, id domain.LinkedIdentityID) error
}
