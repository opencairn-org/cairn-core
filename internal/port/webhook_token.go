package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// WebhookTokenRepo manages the per-account webhook signing secrets stored
// hashed on external_accounts.
type WebhookTokenRepo interface {
	// ListWebhookTokens pages the accounts (all providers when provider is
	// empty) newest first and reports the total.
	ListWebhookTokens(ctx context.Context, provider string, limit, offset int) ([]domain.WebhookSigningToken, int, error)
	// SelectAccountsForRotation returns the accounts matching every given
	// selector; a nil rotatedBefore skips the age check, and an account that
	// never had a token counts as older than any cut-off.
	SelectAccountsForRotation(ctx context.Context, provider string, rotatedBefore *time.Time, ids []domain.ExternalAccountID) ([]domain.ExternalAccountID, error)
	// SetWebhookSigningToken stores the hash and the rotation time.
	SetWebhookSigningToken(ctx context.Context, id domain.ExternalAccountID, hash []byte, at time.Time) error
}
