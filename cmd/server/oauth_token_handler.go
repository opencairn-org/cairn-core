package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// startOAuthTokenHandlers mounts three NATS request/reply handlers that
// expose the OAuth token store to workers. All three are provider-
// agnostic — the subject's provider segment is informational only;
// the actual routing is by accountID.
//
//	cairn.tokens.<provider>.get          → returns current TokenState
//	cairn.tokens.<provider>.store        → atomically updates TokenState
//	cairn.tokens.<provider>.needs_reauth → marks status, emits event
//
// Gated on cfg.NATS.URL; returns nil when no bus is wired.
//
// Returns a slice of subscriptions so serve.go can Close() them on
// shutdown.
func startOAuthTokenHandlers(
	ctx context.Context,
	app *App,
	logger *slog.Logger,
) ([]port.Subscription, error) {
	if app.NATSBus == nil || app.OAuthTokens == nil {
		return nil, nil
	}
	log := logger.With("component", "oauth_token_handler")

	subs := make([]port.Subscription, 0, 3)

	// GET handler
	get, err := app.NATSBus.RespondTo(ctx, "cairn.tokens.*.get",
		func(ctx context.Context, body []byte) ([]byte, error) {
			return handleTokenGet(ctx, app, log, body)
		})
	if err != nil {
		return nil, fmt.Errorf("subscribe tokens.*.get: %w", err)
	}
	subs = append(subs, get)

	// STORE handler
	store, err := app.NATSBus.RespondTo(ctx, "cairn.tokens.*.store",
		func(ctx context.Context, body []byte) ([]byte, error) {
			return handleTokenStore(ctx, app, log, body)
		})
	if err != nil {
		return nil, fmt.Errorf("subscribe tokens.*.store: %w", err)
	}
	subs = append(subs, store)

	// NEEDS_REAUTH handler
	reauth, err := app.NATSBus.RespondTo(ctx, "cairn.tokens.*.needs_reauth",
		func(ctx context.Context, body []byte) ([]byte, error) {
			return handleTokenNeedsReauth(ctx, app, log, body)
		})
	if err != nil {
		return nil, fmt.Errorf("subscribe tokens.*.needs_reauth: %w", err)
	}
	subs = append(subs, reauth)

	log.Info("oauth token handlers active on cairn.tokens.*.{get,store,needs_reauth}")
	return subs, nil
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// tokenRequest is the common shape — every call carries an account_id.
type tokenRequest struct {
	AccountID string `json:"account_id"`
}

// tokenGetReply mirrors port.TokenState plus an error field for
// non-success paths.
type tokenGetReply struct {
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`

	// Per-user OAuth app credentials so the worker can refresh against the
	// provider's token endpoint. Credentials are per-user (stored encrypted in
	// user_provider_configs), not instance-global.
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// AppID names the app (hash of client_id) for per-app rate-limit buckets.
	AppID string `json:"app_id,omitempty"`

	// Error categorises failure paths. Stable strings so worker SDK
	// can map to *port.TerminalError / NakWithDelayError without parsing.
	Error      string `json:"error,omitempty"`       // "account_gone" | "transient" | "needs_reauth"
	RetryAfter int    `json:"retry_after,omitempty"` // seconds, when error=transient
}

type tokenStoreRequest struct {
	AccountID         string    `json:"account_id"`
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
	Scope             string    `json:"scope,omitempty"`
	TokenType         string    `json:"token_type,omitempty"`
	PreviousExpiresAt time.Time `json:"previous_expires_at"`
}

type tokenStoreReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"` // "account_gone" | "stale_store"
}

type tokenReauthRequest struct {
	AccountID string `json:"account_id"`
	Reason    string `json:"reason"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func handleTokenGet(
	ctx context.Context,
	app *App,
	log *slog.Logger,
	body []byte,
) ([]byte, error) {
	var req tokenRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return encodeReply(tokenGetReply{Error: "bad_request"}), nil
	}
	accID, err := uuid.Parse(req.AccountID)
	if err != nil {
		return encodeReply(tokenGetReply{Error: "bad_request"}), nil
	}

	st, err := app.OAuthTokens.GetToken(ctx, domain.ExternalAccountID(accID))
	if errors.Is(err, port.ErrTokenAccountNotFound) {
		log.Debug("token get: account_gone", "account_id", accID)
		return encodeReply(tokenGetReply{Error: "account_gone"}), nil
	}
	if err != nil {
		log.Warn("token get: transient error", "account_id", accID, "error", err)
		return encodeReply(tokenGetReply{Error: "transient", RetryAfter: 30}), nil
	}

	// Attach the account owner's per-user OAuth app credentials so the worker
	// can refresh. Best-effort: if the account or its provider config is gone,
	// the worker still gets the access token (good until expiry) and surfaces
	// needs_reauth on the next refresh attempt.
	clientID, clientSecret := tokenGetClientCreds(ctx, app, log, accID)

	return encodeReply(tokenGetReply{
		AccessToken:  st.AccessToken,
		RefreshToken: st.RefreshToken,
		ExpiresAt:    st.ExpiresAt,
		Scope:        st.Scope,
		TokenType:    st.TokenType,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AppID:        domain.AppID(clientID),
	}), nil
}

// tokenGetClientCreds resolves the account → owning user + provider →
// per-user OAuth app credentials. Returns blanks (not an error) on any miss;
// the caller treats credentials as optional.
func tokenGetClientCreds(ctx context.Context, app *App, log *slog.Logger, accID uuid.UUID) (string, string) {
	if app.ExternalAccounts == nil || app.UserProviderConfigs == nil {
		return "", ""
	}
	acct, err := app.ExternalAccounts.GetExternalAccount(ctx, domain.ExternalAccountID(accID))
	if err != nil {
		log.Debug("token get: account lookup for creds failed", "account_id", accID, "error", err)
		return "", ""
	}
	if acct.ConnectionID == nil {
		log.Debug("token get: account has no connection", "account_id", accID)
		return "", ""
	}
	conn, err := app.UserProviderConfigs.GetByID(ctx, *acct.ConnectionID)
	if err != nil {
		log.Debug("token get: connection not found", "account_id", accID, "connection_id", acct.ConnectionID)
		return "", ""
	}
	return conn.ClientID, conn.ClientSecret
}

func handleTokenStore(
	ctx context.Context,
	app *App,
	log *slog.Logger,
	body []byte,
) ([]byte, error) {
	var req tokenStoreRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return encodeReply(tokenStoreReply{Error: "bad_request"}), nil
	}
	accID, err := uuid.Parse(req.AccountID)
	if err != nil {
		return encodeReply(tokenStoreReply{Error: "bad_request"}), nil
	}
	if req.AccessToken == "" || req.ExpiresAt.IsZero() {
		return encodeReply(tokenStoreReply{Error: "bad_request"}), nil
	}

	in := port.StoreTokenInput{
		State: port.TokenState{
			AccessToken:  req.AccessToken,
			RefreshToken: req.RefreshToken,
			ExpiresAt:    req.ExpiresAt,
			Scope:        req.Scope,
			TokenType:    req.TokenType,
		},
		PreviousExpiresAt: req.PreviousExpiresAt,
	}
	err = app.OAuthTokens.StoreToken(ctx, domain.ExternalAccountID(accID), in)
	switch {
	case errors.Is(err, port.ErrTokenStaleStore):
		log.Debug("token store: stale (concurrent refresh)", "account_id", accID)
		return encodeReply(tokenStoreReply{Error: "stale_store"}), nil
	case errors.Is(err, port.ErrTokenAccountNotFound):
		return encodeReply(tokenStoreReply{Error: "account_gone"}), nil
	case err != nil:
		log.Warn("token store: transient error", "account_id", accID, "error", err)
		return encodeReply(tokenStoreReply{Error: "transient"}), nil
	}

	log.Info("token stored", "account_id", accID, "expires_at", req.ExpiresAt)
	return encodeReply(tokenStoreReply{OK: true}), nil
}

func handleTokenNeedsReauth(
	ctx context.Context,
	app *App,
	log *slog.Logger,
	body []byte,
) ([]byte, error) {
	var req tokenReauthRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return encodeReply(tokenStoreReply{Error: "bad_request"}), nil
	}
	accID, err := uuid.Parse(req.AccountID)
	if err != nil {
		return encodeReply(tokenStoreReply{Error: "bad_request"}), nil
	}
	if req.Reason == "" {
		req.Reason = "refresh_failed"
	}

	if err := app.OAuthTokens.MarkNeedsReauth(ctx, domain.ExternalAccountID(accID), req.Reason); err != nil {
		log.Warn("mark needs_reauth failed", "account_id", accID, "error", err)
		return encodeReply(tokenStoreReply{Error: "transient"}), nil
	}

	// Fire the domain event so user-facing notification dispatchers can
	// react. Best-effort: failure to publish doesn't roll back the DB flip.
	eventBody, _ := json.Marshal(map[string]any{
		"account_id": accID.String(),
		"reason":     req.Reason,
		"at":         time.Now().UTC().Format(time.RFC3339),
	})
	msgID := "reauth:" + accID.String()
	if err := app.NATSBus.Publish(ctx, "cairn.events.external_account.needs_reauth", msgID, eventBody); err != nil {
		log.Warn("publish needs_reauth event failed", "account_id", accID, "error", err)
	}

	log.Info("account marked needs_reauth", "account_id", accID, "reason", req.Reason)
	return encodeReply(tokenStoreReply{OK: true}), nil
}

// encodeReply marshals a reply struct. Errors here are protocol bugs;
// the only sensible response is an empty body and let the requester
// time out.
func encodeReply(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
