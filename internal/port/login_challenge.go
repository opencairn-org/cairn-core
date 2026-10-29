package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// LoginChallengeRepo persists second-factor challenges (login_challenges).
// Callers pass the SHA-256 of the challenge token; consumption is atomic.
type LoginChallengeRepo interface {
	Create(ctx context.Context, c domain.LoginChallenge) (domain.LoginChallengeID, error)
	// GetByTokenHash returns the challenge or domain.ErrNotFound.
	GetByTokenHash(ctx context.Context, hash []byte) (domain.LoginChallenge, error)
	// MarkSatisfied records the verified passkey assertion.
	MarkSatisfied(ctx context.Context, id domain.LoginChallengeID, now time.Time) error
	// Consume turns a satisfied, unexpired, unconsumed challenge into its
	// user id exactly once; anything else is ErrAuthTokenInvalid.
	Consume(ctx context.Context, hash []byte, now time.Time) (domain.UserID, error)
}
