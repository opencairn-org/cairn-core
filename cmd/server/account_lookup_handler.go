package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// startAccountLookupHandler wires the NATS responder that resolves a provider's
// owner/athlete id into an internal account + user id. Workers call this from
// their webhook handlers (a webhook carries only the provider-side owner_id) so
// they can publish a fetch job for the right account. Without it, webhook
// imports time out and fall back to reconcile-polling (hours of latency).
//
//	subject:  cairn.accounts.lookup_by_provider_ext
//	request:  {"provider": "strava", "external_id": "12345"}
//	reply:    {"account_id": "uuid", "user_id": "uuid"} | {"error": "..."}
func startAccountLookupHandler(
	ctx context.Context,
	app *App,
	logger *slog.Logger,
) (port.Subscription, error) {
	if app.NATSBus == nil || app.ExternalAccounts == nil {
		return nil, nil
	}
	log := logger.With("component", "account_lookup_handler")

	sub, err := app.NATSBus.RespondTo(ctx, "cairn.accounts.lookup_by_provider_ext",
		func(ctx context.Context, body []byte) ([]byte, error) {
			return accountLookupRespond(ctx, app, log, body), nil
		})
	if err != nil {
		return nil, err
	}
	// Per-pool subject (docs/adr/0006): the pool is taken from the subject
	// the worker is allowed to publish on, not from the body.
	poolSub, err := app.NATSBus.RespondToSubject(ctx, "cairn.accounts.lookup.*.*",
		func(ctx context.Context, subject string, body []byte) ([]byte, error) {
			parts := strings.Split(subject, ".")
			var req accountLookupRequest
			if json.Unmarshal(body, &req) != nil {
				return accountLookupErr("bad_request"), nil
			}
			req.Provider, req.WorkerName = parts[3], parts[4]
			scoped, _ := json.Marshal(req)
			return accountLookupRespond(ctx, app, log, scoped), nil
		})
	if err != nil {
		_ = sub.Close(ctx)
		return nil, err
	}
	sub = multiSubscription{sub, poolSub}
	log.Info("account lookup handler active on cairn.accounts.lookup_by_provider_ext")
	return sub, nil
}

func accountLookupErr(reason string) []byte {
	b, _ := json.Marshal(map[string]string{"error": reason})
	return b
}

// accountLookupRequest is the cairn.accounts.lookup_by_provider_ext request body.
type accountLookupRequest struct {
	Provider       string `json:"provider"`
	ExternalID     string `json:"external_id"`
	SubscriptionID string `json:"subscription_id"`
	// WorkerName is the asking pool: one provider account may have a
	// connection per pool (docs/adr/0006). Empty (pre-0.5 workers) matches any.
	WorkerName string `json:"worker_name"`
}

// accountLookupRespond resolves one lookup request into its reply payload.
func accountLookupRespond(ctx context.Context, app *App, log *slog.Logger, body []byte) []byte {
	var req accountLookupRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return accountLookupErr("bad_request")
	}
	if req.Provider == "" || (req.ExternalID == "" && req.SubscriptionID == "") {
		return accountLookupErr("provider and external_id or subscription_id required")
	}
	acct, err := accountLookupFind(ctx, app, log, req)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return accountLookupErr("not_found")
		}
		log.Warn("account lookup failed", "provider", req.Provider, "error", err)
		return accountLookupErr("lookup_failed")
	}
	// Auto-import suspended for this account (#93): refuse the lookup so
	// the worker drops the webhook-driven fetch. The account stays
	// linked; manual sync still works (it doesn't go through here).
	if !acct.AutoImportEnabled {
		return accountLookupErr("auto_import_suspended")
	}
	accountLookupSelfHeal(ctx, app, log, req, acct)
	out, _ := json.Marshal(map[string]string{
		"account_id": acct.ID.String(),
		"user_id":    acct.UserID.String(),
	})
	return out
}

// accountLookupFind prefers the unambiguous subscription_id path (#50): the same
// provider athlete may be linked by multiple connections, so owner_id alone can
// resolve to the wrong one. Falls back to owner_id only when the subscription is unknown.
func accountLookupFind(ctx context.Context, app *App, log *slog.Logger, req accountLookupRequest) (domain.ExternalAccount, error) {
	if req.SubscriptionID == "" {
		return app.ExternalAccounts.FindByProviderAndExternalID(ctx, req.Provider, req.ExternalID, req.WorkerName)
	}
	acct, err := app.ExternalAccounts.FindBySubscription(ctx, req.Provider, req.SubscriptionID, req.WorkerName)
	if errors.Is(err, domain.ErrNotFound) && req.ExternalID != "" {
		log.Warn("subscription unknown, falling back to external_id",
			"provider", req.Provider, "subscription_id", req.SubscriptionID)
		acct, err = app.ExternalAccounts.FindByProviderAndExternalID(ctx, req.Provider, req.ExternalID, req.WorkerName)
	}
	return acct, err
}

// accountLookupSelfHeal records the subscription → account mapping (#50): the first
// event that carried a subscription_id but resolved via the owner_id fallback
// records the binding, so every subsequent event resolves via FindBySubscription.
func accountLookupSelfHeal(ctx context.Context, app *App, log *slog.Logger, req accountLookupRequest, acct domain.ExternalAccount) {
	if req.SubscriptionID == "" || acct.WebhookSubscriptionID == req.SubscriptionID {
		return
	}
	if regErr := app.ExternalAccounts.RegisterWebhookSubscription(
		ctx, req.Provider, req.SubscriptionID, acct.ID); regErr != nil {
		log.Warn("self-heal subscription mapping failed",
			"provider", req.Provider, "account_id", acct.ID.String(), "error", regErr)
		return
	}
	log.Info("recorded webhook subscription mapping",
		"provider", req.Provider, "account_id", acct.ID.String(),
		"subscription_id", req.SubscriptionID)
}

// multiSubscription closes several subscriptions as one.
type multiSubscription []port.Subscription

func (m multiSubscription) Close(ctx context.Context) error {
	var err error
	for _, s := range m {
		err = errors.Join(err, s.Close(ctx))
	}
	return err
}
