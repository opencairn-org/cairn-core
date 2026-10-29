// Package webhooks holds the admin side of the per-account webhook signing
// secrets. No worker consumes the secret yet, so a rotation only replaces
// the stored hash; re-registering the provider subscription is a later step.
package webhooks

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

// Tokens lists and bulk-rotates webhook signing tokens.
type Tokens struct {
	Repo  port.WebhookTokenRepo
	Audit *audit.Recorder
	Now   func() time.Time
}

func (t *Tokens) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now().UTC()
}

// List pages the tokens, all providers when provider is empty.
func (t *Tokens) List(ctx context.Context, provider string, limit, offset int) ([]domain.WebhookSigningToken, int, error) {
	if t.Repo == nil {
		return nil, 0, port.NewActionError(port.ActionErrorUnavailable, "webhook tokens are not available")
	}
	rows, total, err := t.Repo.ListWebhookTokens(ctx, provider, limit, offset)
	if err != nil {
		return nil, 0, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("list webhook tokens: %v", err))
	}
	return rows, total, nil
}

// BulkRotate mints a new token for every account the selector matches and
// reports how many were rotated; an empty selector is refused.
func (t *Tokens) BulkRotate(ctx context.Context, actor audit.Actor, sel domain.WebhookRotationSelector) ([]domain.ExternalAccountID, error) {
	if t.Repo == nil {
		return nil, port.NewActionError(port.ActionErrorUnavailable, "webhook tokens are not available")
	}
	if err := sel.Validate(); err != nil {
		return nil, port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	now := t.now()
	var before *time.Time
	if sel.OlderThan != nil {
		cut := now.Add(-*sel.OlderThan)
		before = &cut
	}
	ids, err := t.Repo.SelectAccountsForRotation(ctx, sel.Provider, before, sel.AccountIDs)
	if err != nil {
		return nil, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("select accounts: %v", err))
	}
	rotated := make([]domain.ExternalAccountID, 0, len(ids))
	for _, id := range ids {
		_, hash := domain.NewWebhookSigningToken()
		if err := t.Repo.SetWebhookSigningToken(ctx, id, hash, now); err != nil {
			return rotated, port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("rotate %s: %v", id, err))
		}
		rotated = append(rotated, id)
	}
	t.Audit.Info(ctx, actor, domain.AuditWebhookTokensRotated, "external_account", "",
		map[string]string{"provider": sel.Provider, "rotated": strconv.Itoa(len(rotated))})
	return rotated, nil
}
