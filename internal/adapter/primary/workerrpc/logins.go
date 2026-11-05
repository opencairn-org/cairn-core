package workerrpc

import (
	"context"
	"errors"
	"log/slog"

	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/providerlink"
)

// Logins serves cairn.creds.<provider>.get for credential providers:
// account_id → the connection's login pair. The worker asks per job;
// credentials are never queued. The provider comes from the subject.
type Logins struct {
	Tokens *providerlink.WorkerTokens
	Log    *slog.Logger
}

// Start subscribes the responder.
func (l *Logins) Start(ctx context.Context, bus port.JobBus) (port.Subscription, error) {
	sub, err := bus.RespondToSubject(ctx, "cairn.creds.*.get", l.get)
	if err != nil {
		return nil, err
	}
	l.Log.Info("credential handler active on cairn.creds.*.get")
	return sub, nil
}

func (l *Logins) get(ctx context.Context, subject string, body []byte) ([]byte, error) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	provider := providerOf(subject)
	accID, ok := accountOf(body, &req, func() string { return req.AccountID })
	if !ok || provider == "" {
		return encode(map[string]any{"error": "bad_request"}), nil
	}
	login, secret, err := l.Tokens.Login(ctx, provider, accID)
	switch {
	case errors.Is(err, providerlink.ErrWrongProvider):
		l.Log.Warn("creds get: provider mismatch", "subject", subject, "account_id", accID)
		return encode(map[string]any{"error": "account_gone"}), nil
	case errors.Is(err, providerlink.ErrAccountGone):
		return encode(map[string]any{"error": "account_gone"}), nil
	case errors.Is(err, providerlink.ErrNeedsReauth):
		return encode(map[string]any{"error": "needs_reauth"}), nil
	case err != nil:
		return encode(map[string]any{"error": "transient"}), nil
	}
	// email/password: aliases read by the Garmin worker ≤0.6; drop in 0.8.
	return encode(map[string]any{
		"client_id": login, "client_secret": secret,
		"email": login, "password": secret,
	}), nil
}
