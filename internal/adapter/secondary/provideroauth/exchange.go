// Package provideroauth talks to provider OAuth token endpoints
// (port.ProviderCodeExchanger).
package provideroauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Exchanger posts authorization codes to a provider's token endpoint.
type Exchanger struct {
	// Client defaults to one with a 15s timeout.
	Client *http.Client
}

var _ port.ProviderCodeExchanger = (*Exchanger)(nil)

// ExchangeCode trades code for tokens with the connection's app credentials.
func (e *Exchanger) ExchangeCode(ctx context.Context, tokenURL string, conn domain.UserProviderConfig, code string) (port.TokenState, string, error) {
	form := url.Values{}
	form.Set("client_id", conn.ClientID)
	form.Set("client_secret", conn.ClientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return port.TokenState{}, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return port.TokenState{}, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return port.TokenState{}, "", fmt.Errorf("token endpoint %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		Athlete      struct {
			ID int64 `json:"id"`
		} `json:"athlete"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return port.TokenState{}, "", err
	}
	exp := time.Unix(body.ExpiresAt, 0)
	if body.ExpiresAt == 0 && body.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	ts := port.TokenState{
		AccessToken: body.AccessToken, RefreshToken: body.RefreshToken,
		ExpiresAt: exp, Scope: body.Scope, TokenType: body.TokenType,
	}
	acctID := ""
	if body.Athlete.ID != 0 {
		acctID = strconv.FormatInt(body.Athlete.ID, 10)
	}
	return ts, acctID, nil
}
