package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// OIDC providers are configured via env (CAIRN_OIDC_*) and held in memory —
// there is no client repository. The only persisted OIDC state is the
// user↔provider link below.

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
