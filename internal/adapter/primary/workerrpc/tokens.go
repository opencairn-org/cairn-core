// Package workerrpc is the request/reply primary adapter provider workers
// call over the job bus: it decodes their JSON requests, runs the use case
// and encodes a reply with stable error codes the worker SDK maps to its
// retry policy.
package workerrpc

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
	"github.com/opencairn-org/cairn-core/internal/usecase/providerlink"
)

// Tokens serves the OAuth token store to workers. The subject's provider
// segment is informational; routing is by account id.
//
//	cairn.tokens.<provider>.get          → current token state + app credentials
//	cairn.tokens.<provider>.store        → optimistic token update
//	cairn.tokens.<provider>.needs_reauth → flag the account, emit the event
type Tokens struct {
	Tokens *providerlink.WorkerTokens
	Log    *slog.Logger
}

// Start subscribes the three responders.
func (t *Tokens) Start(ctx context.Context, bus port.JobBus) ([]port.Subscription, error) {
	handlers := []struct {
		subject string
		fn      port.RequestHandler
	}{
		{"cairn.tokens.*.get", t.get},
		{"cairn.tokens.*.store", t.store},
		{"cairn.tokens.*.needs_reauth", t.needsReauth},
	}
	subs := make([]port.Subscription, 0, len(handlers))
	for _, h := range handlers {
		sub, err := bus.RespondTo(ctx, h.subject, h.fn)
		if err != nil {
			for _, s := range subs {
				_ = s.Close(ctx)
			}
			return nil, fmt.Errorf("subscribe %s: %w", h.subject, err)
		}
		subs = append(subs, sub)
	}
	t.Log.Info("oauth token handlers active on cairn.tokens.*.{get,store,needs_reauth}")
	return subs, nil
}

// tokenGetReply mirrors port.TokenState plus the per-user app credentials.
type tokenGetReply struct {
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
	// AppID names the app (hash of client_id) for per-app rate-limit buckets.
	AppID string `json:"app_id,omitempty"`
	// Error: "account_gone" | "transient" | "bad_request".
	Error      string `json:"error,omitempty"`
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
	Error string `json:"error,omitempty"` // "account_gone" | "stale_store" | "transient" | "bad_request"
}

// accountOf decodes the request and its account id.
func accountOf(body []byte, dst any, id func() string) (domain.ExternalAccountID, bool) {
	if err := json.Unmarshal(body, dst); err != nil {
		return domain.ExternalAccountID{}, false
	}
	u, err := uuid.Parse(id())
	if err != nil {
		return domain.ExternalAccountID{}, false
	}
	return domain.ExternalAccountID(u), true
}

func (t *Tokens) get(ctx context.Context, body []byte) ([]byte, error) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	accID, ok := accountOf(body, &req, func() string { return req.AccountID })
	if !ok {
		return encode(tokenGetReply{Error: "bad_request"}), nil
	}
	tok, err := t.Tokens.Get(ctx, accID)
	if errors.Is(err, port.ErrTokenAccountNotFound) {
		t.Log.Debug("token get: account_gone", "account_id", accID)
		return encode(tokenGetReply{Error: "account_gone"}), nil
	}
	if err != nil {
		t.Log.Warn("token get: transient error", "account_id", accID, "error", err)
		return encode(tokenGetReply{Error: "transient", RetryAfter: 30}), nil
	}
	return encode(tokenGetReply{
		AccessToken: tok.State.AccessToken, RefreshToken: tok.State.RefreshToken, ExpiresAt: tok.State.ExpiresAt,
		Scope: tok.State.Scope, TokenType: tok.State.TokenType,
		ClientID: tok.ClientID, ClientSecret: tok.ClientSecret, AppID: domain.AppID(tok.ClientID),
	}), nil
}

func (t *Tokens) store(ctx context.Context, body []byte) ([]byte, error) {
	var req tokenStoreRequest
	accID, ok := accountOf(body, &req, func() string { return req.AccountID })
	if !ok || req.AccessToken == "" || req.ExpiresAt.IsZero() {
		return encode(tokenStoreReply{Error: "bad_request"}), nil
	}
	err := t.Tokens.Store(ctx, accID, port.StoreTokenInput{
		State: port.TokenState{
			AccessToken: req.AccessToken, RefreshToken: req.RefreshToken, ExpiresAt: req.ExpiresAt,
			Scope: req.Scope, TokenType: req.TokenType,
		},
		PreviousExpiresAt: req.PreviousExpiresAt,
	})
	switch {
	case errors.Is(err, port.ErrTokenStaleStore):
		t.Log.Debug("token store: stale (concurrent refresh)", "account_id", accID)
		return encode(tokenStoreReply{Error: "stale_store"}), nil
	case errors.Is(err, port.ErrTokenAccountNotFound):
		return encode(tokenStoreReply{Error: "account_gone"}), nil
	case err != nil:
		t.Log.Warn("token store: transient error", "account_id", accID, "error", err)
		return encode(tokenStoreReply{Error: "transient"}), nil
	}
	t.Log.Info("token stored", "account_id", accID, "expires_at", req.ExpiresAt)
	return encode(tokenStoreReply{OK: true}), nil
}

func (t *Tokens) needsReauth(ctx context.Context, body []byte) ([]byte, error) {
	var req struct {
		AccountID string `json:"account_id"`
		Reason    string `json:"reason"`
	}
	accID, ok := accountOf(body, &req, func() string { return req.AccountID })
	if !ok {
		return encode(tokenStoreReply{Error: "bad_request"}), nil
	}
	if err := t.Tokens.MarkNeedsReauth(ctx, accID, req.Reason); err != nil {
		t.Log.Warn("mark needs_reauth failed", "account_id", accID, "error", err)
		return encode(tokenStoreReply{Error: "transient"}), nil
	}
	return encode(tokenStoreReply{OK: true}), nil
}

// encode marshals a reply; a failure is a protocol bug, the requester then
// times out on the empty body.
func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
