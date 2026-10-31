package workerrpc

import (
	"context"
	"errors"
	"log/slog"

	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/providerlink"
)

// GarminLogins serves cairn.creds.garmin.get: account_id → {email, password}.
// The worker asks per job; credentials are never queued.
type GarminLogins struct {
	Tokens *providerlink.WorkerTokens
	Log    *slog.Logger
}

// Start subscribes the responder.
func (g *GarminLogins) Start(ctx context.Context, bus port.JobBus) (port.Subscription, error) {
	sub, err := bus.RespondTo(ctx, "cairn.creds.garmin.get", g.get)
	if err != nil {
		return nil, err
	}
	g.Log.Info("garmin creds handler active on cairn.creds.garmin.get")
	return sub, nil
}

func (g *GarminLogins) get(ctx context.Context, body []byte) ([]byte, error) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	accID, ok := accountOf(body, &req, func() string { return req.AccountID })
	if !ok {
		return encode(map[string]any{"error": "bad_request"}), nil
	}
	email, password, err := g.Tokens.Login(ctx, accID)
	switch {
	case errors.Is(err, providerlink.ErrAccountGone):
		return encode(map[string]any{"error": "account_gone"}), nil
	case errors.Is(err, providerlink.ErrNeedsReauth):
		return encode(map[string]any{"error": "needs_reauth"}), nil
	case err != nil:
		return encode(map[string]any{"error": "transient"}), nil
	}
	return encode(map[string]any{"email": email, "password": password}), nil
}
