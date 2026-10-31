package sync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// AccountLookup resolves a provider's owner/athlete id (or push
// subscription) into the internal account + user, for workers routing
// webhook events. Without it webhook imports fall back to reconcile polling.
//
//	request:  {"provider": "strava", "external_id": "12345", "subscription_id": "..."}
//	reply:    {"account_id": "uuid", "user_id": "uuid"} | {"error": "..."}
type AccountLookup struct {
	Accounts port.ExternalAccountRepo
	Log      *slog.Logger
}

// LookupRequest is the lookup request body.
type LookupRequest struct {
	Provider       string `json:"provider"`
	ExternalID     string `json:"external_id"`
	SubscriptionID string `json:"subscription_id"`
	// WorkerName is the asking pool: one provider account may have a
	// connection per pool (docs/adr/0006). Empty (pre-0.5 workers) matches any.
	WorkerName string `json:"worker_name"`
}

func lookupErr(reason string) []byte {
	b, _ := json.Marshal(map[string]string{"error": reason})
	return b
}

func (l *AccountLookup) log() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.Default()
}

// RespondPool answers a request on the per-pool subject
// cairn.accounts.lookup.<provider>.<pool>: provider and pool come from the
// subject the worker may publish on, never from the body.
func (l *AccountLookup) RespondPool(ctx context.Context, subject string, body []byte) []byte {
	parts := strings.Split(subject, ".")
	var req LookupRequest
	if len(parts) < 5 || json.Unmarshal(body, &req) != nil {
		return lookupErr("bad_request")
	}
	req.Provider, req.WorkerName = parts[3], parts[4]
	return l.respond(ctx, req)
}

// Respond answers one raw request body.
func (l *AccountLookup) Respond(ctx context.Context, body []byte) []byte {
	var req LookupRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return lookupErr("bad_request")
	}
	return l.respond(ctx, req)
}

func (l *AccountLookup) respond(ctx context.Context, req LookupRequest) []byte {
	if req.Provider == "" || (req.ExternalID == "" && req.SubscriptionID == "") {
		return lookupErr("provider and external_id or subscription_id required")
	}
	acct, err := l.find(ctx, req)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return lookupErr("not_found")
		}
		l.log().Warn("account lookup failed", "provider", req.Provider, "error", err)
		return lookupErr("lookup_failed")
	}
	// Auto-import suspended (#93): the worker drops the webhook-driven fetch;
	// manual sync doesn't come through here.
	if !acct.AutoImportEnabled {
		return lookupErr("auto_import_suspended")
	}
	l.selfHeal(ctx, req, acct)
	out, _ := json.Marshal(map[string]string{"account_id": acct.ID.String(), "user_id": acct.UserID.String()})
	return out
}

// find prefers the unambiguous subscription_id (#50): one provider athlete
// may be linked by several connections. owner_id only when the subscription is unknown.
func (l *AccountLookup) find(ctx context.Context, req LookupRequest) (domain.ExternalAccount, error) {
	if req.SubscriptionID == "" {
		return l.Accounts.FindByProviderAndExternalID(ctx, req.Provider, req.ExternalID, req.WorkerName)
	}
	acct, err := l.Accounts.FindBySubscription(ctx, req.Provider, req.SubscriptionID, req.WorkerName)
	if errors.Is(err, domain.ErrNotFound) && req.ExternalID != "" {
		l.log().Warn("subscription unknown, falling back to external_id",
			"provider", req.Provider, "subscription_id", req.SubscriptionID)
		acct, err = l.Accounts.FindByProviderAndExternalID(ctx, req.Provider, req.ExternalID, req.WorkerName)
	}
	return acct, err
}

// selfHeal records the subscription → account binding the first time an
// event resolved via the owner_id fallback (#50).
func (l *AccountLookup) selfHeal(ctx context.Context, req LookupRequest, acct domain.ExternalAccount) {
	if req.SubscriptionID == "" || acct.WebhookSubscriptionID == req.SubscriptionID {
		return
	}
	if err := l.Accounts.RegisterWebhookSubscription(ctx, req.Provider, req.SubscriptionID, acct.ID); err != nil {
		l.log().Warn("self-heal subscription mapping failed",
			"provider", req.Provider, "account_id", acct.ID.String(), "error", err)
		return
	}
	l.log().Info("recorded webhook subscription mapping",
		"provider", req.Provider, "account_id", acct.ID.String(), "subscription_id", req.SubscriptionID)
}
