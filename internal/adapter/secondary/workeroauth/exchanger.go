// Package workeroauth delegates the OAuth authorization-code exchange to the
// provider's worker pool over the job bus, so every provider quirk (client
// auth style, PKCE, non-standard token endpoints, where the account id
// lives) stays in the worker.
package workeroauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Exchanger asks cairn.oauth.<provider>.<pool>.exchange.
type Exchanger struct {
	Bus port.JobBus
	// Timeout bounds the round-trip (the worker calls the provider); 20s
	// when zero.
	Timeout time.Duration
}

var _ port.ProviderCodeExchanger = (*Exchanger)(nil)

// ErrNoBus: the instance has no job bus, so no worker can exchange.
var ErrNoBus = errors.New("oauth exchange: no job bus wired")

type exchangeRequest struct {
	ConnectionID string `json:"connection_id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	CodeVerifier string `json:"code_verifier,omitempty"`
}

type exchangeReply struct {
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token"`
	ExpiresAt         time.Time `json:"expires_at"`
	Scope             string    `json:"scope"`
	TokenType         string    `json:"token_type"`
	ProviderAccountID string    `json:"provider_account_id"`
	// Error: "exchange_failed" | "bad_request" | "transient"; Message explains.
	Error   string `json:"error"`
	Message string `json:"message"`
}

// ExchangeCode implements port.ProviderCodeExchanger.
func (e *Exchanger) ExchangeCode(ctx context.Context, provider, pool string, conn domain.UserProviderConfig, in port.CodeExchange) (port.TokenState, string, error) {
	if e.Bus == nil {
		return port.TokenState{}, "", ErrNoBus
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	body, err := json.Marshal(exchangeRequest{ //nolint:gosec // G117: the pool needs the app secret to exchange the code; the bus is the trusted channel
		ConnectionID: conn.ID.String(), ClientID: conn.ClientID, ClientSecret: conn.ClientSecret,
		Code: in.Code, RedirectURI: in.RedirectURI, CodeVerifier: in.CodeVerifier,
	})
	if err != nil {
		return port.TokenState{}, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := e.Bus.Request(ctx, domain.OAuthExchangeSubject(provider, pool), body, timeout)
	if err != nil {
		return port.TokenState{}, "", fmt.Errorf("exchange request to pool %s: %w", pool, err)
	}
	var reply exchangeReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return port.TokenState{}, "", fmt.Errorf("decode exchange reply: %w", err)
	}
	if reply.Error != "" {
		return port.TokenState{}, "", fmt.Errorf("exchange refused by pool %s: %s: %s", pool, reply.Error, reply.Message)
	}
	if reply.AccessToken == "" {
		return port.TokenState{}, "", errors.New("exchange reply without access_token")
	}
	return port.TokenState{
		AccessToken: reply.AccessToken, RefreshToken: reply.RefreshToken, ExpiresAt: reply.ExpiresAt,
		Scope: reply.Scope, TokenType: reply.TokenType,
	}, reply.ProviderAccountID, nil
}
