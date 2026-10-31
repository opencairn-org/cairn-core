// Package oidc implements port.OIDCClient with coreos/go-oidc and x/oauth2.
package oidc

import (
	"context"
	"fmt"
	"slices"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Client runs discovery on every call; it holds no state.
type Client struct{}

var _ port.OIDCClient = Client{}

// New returns the go-oidc backed client.
func New() Client { return Client{} }

func discover(ctx context.Context, p domain.OIDCProvider) (*gooidc.Provider, error) {
	prov, err := gooidc.NewProvider(ctx, p.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc provider discovery: %w", err)
	}
	return prov, nil
}

func config(p domain.OIDCProvider, endpoint oauth2.Endpoint, redirectURL string) *oauth2.Config {
	cfg := &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Scopes:       slices.Clone(p.Scopes),
		Endpoint:     endpoint,
		RedirectURL:  redirectURL,
	}
	if !slices.Contains(cfg.Scopes, "openid") {
		cfg.Scopes = append([]string{"openid"}, cfg.Scopes...)
	}
	return cfg
}

// AuthCodeURL builds the authorize URL with state, nonce and optional PKCE.
func (Client) AuthCodeURL(ctx context.Context, p domain.OIDCProvider, req port.OIDCAuthRequest) (string, error) {
	prov, err := discover(ctx, p)
	if err != nil {
		return "", err
	}
	opts := []oauth2.AuthCodeOption{gooidc.Nonce(req.Nonce)}
	if req.CodeChallenge != "" {
		opts = append(opts,
			oauth2.SetAuthURLParam("code_challenge", req.CodeChallenge),
			oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		)
	}
	return config(p, prov.Endpoint(), req.RedirectURL).AuthCodeURL(req.State, opts...), nil
}

// Exchange redeems the code and verifies the returned id_token.
func (Client) Exchange(ctx context.Context, p domain.OIDCProvider, redirectURL, code, codeVerifier string) (domain.OIDCIDToken, error) {
	prov, err := discover(ctx, p)
	if err != nil {
		return domain.OIDCIDToken{}, err
	}
	var opts []oauth2.AuthCodeOption
	if codeVerifier != "" {
		opts = append(opts, oauth2.SetAuthURLParam("code_verifier", codeVerifier))
	}
	token, err := config(p, prov.Endpoint(), redirectURL).Exchange(ctx, code, opts...)
	if err != nil {
		return domain.OIDCIDToken{}, fmt.Errorf("%w: %w", port.ErrOIDCCodeExchange, err)
	}
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		return domain.OIDCIDToken{}, fmt.Errorf("%w: no id_token in response", port.ErrOIDCIDTokenInvalid)
	}
	idToken, err := prov.Verifier(&gooidc.Config{
		ClientID:          p.EffectiveAudience(),
		SkipClientIDCheck: p.SkipAudienceCheck,
	}).Verify(ctx, raw)
	if err != nil {
		return domain.OIDCIDToken{}, fmt.Errorf("%w: %w", port.ErrOIDCIDTokenInvalid, err)
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return domain.OIDCIDToken{}, fmt.Errorf("decode claims: %w", err)
	}
	return domain.OIDCIDToken{Subject: idToken.Subject, Nonce: idToken.Nonce, Claims: claims}, nil
}
