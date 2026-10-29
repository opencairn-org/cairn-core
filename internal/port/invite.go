package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// InviteRepo persists signup invites. Codes are stored only as a SHA-256 hash;
// the plaintext is never persisted.
type InviteRepo interface {
	// Create inserts a new invite (code already hashed by the caller).
	Create(ctx context.Context, inv domain.Invite, codeHash []byte) (domain.InviteID, error)

	// ClaimByCodeHash atomically marks an invite used (sets used_at) IF it is
	// currently redeemable — not used, revoked, or expired at `now` — and
	// returns the claimed invite (carrying assigned_role + email). Returns
	// domain.ErrInviteInvalid when nothing redeemable matched (unknown code,
	// already used, expired, or revoked — deliberately indistinguishable). This
	// is the race-safe redeem primitive: only one concurrent caller wins.
	ClaimByCodeHash(ctx context.Context, codeHash []byte, now time.Time) (domain.Invite, error)

	// SetUsedBy records which user redeemed the invite (called after the user
	// row is created).
	SetUsedBy(ctx context.Context, id domain.InviteID, userID domain.UserID) error

	// Release reverts a claim (used_at/used_by back to NULL) when the
	// subsequent user creation fails, so the code can be retried.
	Release(ctx context.Context, id domain.InviteID) error

	// Get returns one invite; ErrInviteNotFound when absent.
	Get(ctx context.Context, id domain.InviteID) (domain.Invite, error)

	// ResetCode replaces the code hash and prefix of an unused, unrevoked
	// invite (resend); ErrInviteInvalid when it cannot be reissued.
	ResetCode(ctx context.Context, id domain.InviteID, codeHash []byte, prefix string, now time.Time) error

	// ListForInstance returns all invites (newest first) for the admin UI.
	ListForInstance(ctx context.Context) ([]domain.Invite, error)

	// Revoke marks an invite revoked.
	Revoke(ctx context.Context, id domain.InviteID, now time.Time) error
}
