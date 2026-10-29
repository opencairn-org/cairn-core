package connect

import (
	"context"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/usecase/accounts"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
	"github.com/opencairn-org/cairn-core/internal/usecase/webhooks"
)

// Lifecycle RPCs on ExternalAccountService: removal and per-account webhook
// token rotation. Both resolve ownership through owned() first.

// WithLifecycle attaches the removal and rotation use-cases.
func (s *ExternalAccountServer) WithLifecycle(remover *accounts.Remover, tokens *webhooks.Tokens) *ExternalAccountServer {
	s.Remover, s.Tokens = remover, tokens
	return s
}

func (s *ExternalAccountServer) DeleteExternalAccount(ctx context.Context, req *v1.DeleteExternalAccountRequest) (*v1.DeleteExternalAccountResponse, error) {
	acct, _, err := s.owned(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if s.Remover == nil {
		return nil, unavailable("account removal")
	}
	if _, err := s.Remover.Remove(ctx, audit.Actor{UserID: &acct.UserID}, acct, req.GetDeleteExclusiveActivities()); err != nil {
		return nil, actionErr(err)
	}
	return &v1.DeleteExternalAccountResponse{}, nil
}

// RotateWebhookToken replaces the account's signing secret. Subscriptions
// are worker-owned, so no unregister/register job is queued and both ids
// stay empty.
func (s *ExternalAccountServer) RotateWebhookToken(ctx context.Context, req *v1.RotateWebhookTokenRequest) (*v1.RotateWebhookTokenResponse, error) {
	acct, _, err := s.owned(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if s.Tokens == nil {
		return nil, unavailable("webhook tokens")
	}
	if err := s.Tokens.Rotate(ctx, audit.Actor{UserID: &acct.UserID}, acct.ID); err != nil {
		return nil, actionErr(err)
	}
	return &v1.RotateWebhookTokenResponse{}, nil
}
