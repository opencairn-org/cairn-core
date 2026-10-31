// Package oauthserver is Cairn's OAuth 2.1 authorization server: client
// registration (RFC 7591), authorization code + PKCE, refresh-token rotation,
// the device grant (RFC 8628) and revocation. The transport (form/JSON
// encoding, redirects, status codes) is the primary adapter's business.
package oauthserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Config holds the lifetimes the server issues with.
type Config struct {
	AuthCodeTTL        time.Duration
	AccessTokenTTL     time.Duration
	RefreshTokenTTL    time.Duration
	DeviceCodeTTL      time.Duration
	DevicePollInterval time.Duration
}

// Server runs the authorization-server flows over the OAuth repository.
type Server struct {
	Repo   port.OAuthServerRepo
	Config Config
	Now    func() time.Time
}

// Error is an OAuth protocol error (RFC 6749 §5.2): Code is the wire error
// code, Err the underlying cause of a server_error.
type Error struct {
	Code        string
	Description string
	Err         error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Description + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Description
}

func (e *Error) Unwrap() error { return e.Err }

func protoErr(code, desc string) error { return &Error{Code: code, Description: desc} }

func serverErr(desc string, err error) error {
	return &Error{Code: "server_error", Description: desc, Err: err}
}

var (
	// ErrUnknownClient: the client_id is not registered.
	ErrUnknownClient = errors.New("unknown client_id")
	// ErrInvalidRedirect: the redirect_uri is not one the client registered.
	ErrInvalidRedirect = errors.New("invalid redirect_uri")
	// ErrMissingChallenge: an approval without a PKCE code_challenge.
	ErrMissingChallenge = errors.New("missing code_challenge")
	errInvalidClient    = protoErr("invalid_client", "client authentication failed")
)

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// ClientCredentials is what a token-endpoint caller presents.
type ClientCredentials struct {
	ID     string
	Secret string
}

// TokenGrant is a freshly issued access/refresh pair.
type TokenGrant struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    time.Duration
	Scope        string
}

// authenticate resolves and authenticates the calling client. Public clients
// need no secret: PKCE (or the device code) is the proof.
func (s *Server) authenticate(ctx context.Context, creds ClientCredentials) (domain.OAuthClient, error) {
	if creds.ID == "" {
		return domain.OAuthClient{}, errInvalidClient
	}
	client, err := s.Repo.GetClient(ctx, creds.ID)
	if err != nil {
		return domain.OAuthClient{}, errInvalidClient
	}
	if !client.IsPublic() && !client.VerifySecret(creds.Secret) {
		return domain.OAuthClient{}, errInvalidClient
	}
	return client, nil
}

// registeredClient loads a client and the registered form of redirectURI.
func (s *Server) registeredClient(ctx context.Context, clientID, redirectURI string) (domain.OAuthClient, string, error) {
	client, err := s.Repo.GetClient(ctx, clientID)
	if err != nil {
		return domain.OAuthClient{}, "", ErrUnknownClient
	}
	registered, ok := client.RegisteredRedirectURI(redirectURI)
	if !ok {
		return domain.OAuthClient{}, "", ErrInvalidRedirect
	}
	return client, registered, nil
}

// AuthorizeRequest is the query of GET /oauth/authorize.
type AuthorizeRequest struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	CodeChallenge       string
	CodeChallengeMethod string
}

// Authorize validates an authorization request. It returns the registered
// redirect URI once that can be trusted; ErrUnknownClient/ErrInvalidRedirect
// must be shown to the user, an *Error is reported back to the redirect URI.
func (s *Server) Authorize(ctx context.Context, req AuthorizeRequest) (string, error) {
	client, redirectURI, err := s.registeredClient(ctx, req.ClientID, req.RedirectURI)
	if err != nil {
		return "", err
	}
	switch {
	case req.ResponseType != "code":
		return redirectURI, protoErr("unsupported_response_type", "only response_type=code is supported")
	case !client.AllowsGrant("authorization_code"):
		return redirectURI, protoErr("unauthorized_client", "client may not use the authorization_code grant")
	case req.CodeChallenge == "":
		return redirectURI, protoErr("invalid_request", "PKCE code_challenge is required")
	case req.CodeChallengeMethod != "" && req.CodeChallengeMethod != "S256":
		return redirectURI, protoErr("invalid_request", "only S256 code_challenge_method is supported")
	}
	return redirectURI, nil
}

// Consent is what the consent page shows: the client and the scopes it would
// get out of the requested ones.
func (s *Server) Consent(ctx context.Context, clientID, scope string) (domain.OAuthClient, []string, error) {
	client, err := s.Repo.GetClient(ctx, clientID)
	if err != nil {
		return domain.OAuthClient{}, nil, ErrUnknownClient
	}
	return client, domain.FilterRequestedScopes(domain.ParseScopes(scope), client.Scopes), nil
}

// Decision is the user's answer on the consent page.
type Decision struct {
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Approve             bool
}

// Decide records the consent decision and returns the URL to send the user
// back to: a one-time code on approval, access_denied otherwise.
func (s *Server) Decide(ctx context.Context, userID domain.UserID, d Decision) (string, error) {
	client, redirectURI, err := s.registeredClient(ctx, d.ClientID, d.RedirectURI)
	if err != nil {
		return "", err
	}
	if !d.Approve {
		return AppendQuery(redirectURI, map[string]string{"error": "access_denied", "state": d.State}), nil
	}
	if d.CodeChallenge == "" {
		return "", ErrMissingChallenge
	}
	scopes := domain.FilterRequestedScopes(domain.ParseScopes(d.Scope), client.Scopes)
	code, codeHash := newToken(domain.OAuthAuthCodePrefix)
	method := d.CodeChallengeMethod
	if method == "" {
		method = "S256"
	}
	err = s.Repo.CreateAuthCode(ctx, domain.OAuthAuthorizationCode{
		ClientID:            client.ClientID,
		UserID:              userID,
		RedirectURI:         redirectURI,
		Scope:               domain.JoinScopes(scopes),
		CodeChallenge:       d.CodeChallenge,
		CodeChallengeMethod: method,
		ExpiresAt:           s.now().Add(s.Config.AuthCodeTTL),
	}, codeHash)
	if err != nil {
		return "", fmt.Errorf("create auth code: %w", err)
	}
	return AppendQuery(redirectURI, map[string]string{"code": code, "state": d.State}), nil
}

// ExchangeCode redeems an authorization code (with its PKCE verifier).
func (s *Server) ExchangeCode(ctx context.Context, creds ClientCredentials, code, redirectURI, verifier string) (TokenGrant, error) {
	client, err := s.authenticate(ctx, creds)
	if err != nil {
		return TokenGrant{}, err
	}
	if !strings.HasPrefix(code, domain.OAuthAuthCodePrefix) {
		return TokenGrant{}, protoErr("invalid_grant", "invalid code")
	}
	stored, err := s.Repo.ConsumeAuthCode(ctx, HashToken(code), s.now())
	switch {
	case err != nil:
		return TokenGrant{}, protoErr("invalid_grant", "code invalid, expired, or already used")
	case stored.ClientID != client.ClientID:
		return TokenGrant{}, protoErr("invalid_grant", "code was issued to a different client")
	case stored.RedirectURI != redirectURI:
		return TokenGrant{}, protoErr("invalid_grant", "redirect_uri mismatch")
	case !domain.VerifyPKCE(verifier, stored.CodeChallenge, stored.CodeChallengeMethod):
		return TokenGrant{}, protoErr("invalid_grant", "PKCE verification failed")
	}
	return s.issue(ctx, client.ClientID, stored.UserID, stored.Scope)
}

// Refresh rotates a refresh token: the presented one is revoked and a fresh
// pair issued. scope may narrow the grant, never widen it.
func (s *Server) Refresh(ctx context.Context, creds ClientCredentials, refreshToken, scope string) (TokenGrant, error) {
	client, err := s.authenticate(ctx, creds)
	if err != nil {
		return TokenGrant{}, err
	}
	if !strings.HasPrefix(refreshToken, domain.OAuthRefreshTokenPrefix) {
		return TokenGrant{}, protoErr("invalid_grant", "invalid refresh_token")
	}
	hash := HashToken(refreshToken)
	stored, err := s.Repo.FindRefreshToken(ctx, hash)
	if err != nil || !stored.IsValidAt(s.now()) || stored.ClientID != client.ClientID {
		return TokenGrant{}, protoErr("invalid_grant", "refresh_token invalid or expired")
	}
	_ = s.Repo.RevokeRefreshToken(ctx, hash) // best-effort: the new pair is issued either way
	granted := stored.Scope
	if scope != "" {
		granted = domain.JoinScopes(domain.FilterRequestedScopes(domain.ParseScopes(scope), domain.ParseScopes(stored.Scope)))
	}
	return s.issue(ctx, client.ClientID, stored.UserID, granted)
}

// issue mints and persists an access/refresh pair.
func (s *Server) issue(ctx context.Context, clientID string, userID domain.UserID, scope string) (TokenGrant, error) {
	now := s.now()
	at, atHash := newToken(domain.OAuthAccessTokenPrefix)
	rt, rtHash := newToken(domain.OAuthRefreshTokenPrefix)
	if err := s.Repo.CreateAccessToken(ctx, domain.OAuthAccessToken{
		ClientID: clientID, UserID: userID, Scope: scope, ExpiresAt: now.Add(s.Config.AccessTokenTTL),
	}, atHash); err != nil {
		return TokenGrant{}, serverErr("could not issue token", fmt.Errorf("create access token: %w", err))
	}
	if err := s.Repo.CreateRefreshToken(ctx, domain.OAuthRefreshToken{
		ClientID: clientID, UserID: userID, Scope: scope, ExpiresAt: now.Add(s.Config.RefreshTokenTTL),
	}, rtHash); err != nil {
		return TokenGrant{}, serverErr("could not issue token", fmt.Errorf("create refresh token: %w", err))
	}
	return TokenGrant{AccessToken: at, RefreshToken: rt, ExpiresIn: s.Config.AccessTokenTTL, Scope: scope}, nil
}

// Revoke revokes an access or refresh token; unknown tokens are ignored
// (RFC 7009).
func (s *Server) Revoke(ctx context.Context, token string) {
	switch {
	case strings.HasPrefix(token, domain.OAuthAccessTokenPrefix):
		_ = s.Repo.RevokeAccessToken(ctx, HashToken(token))
	case strings.HasPrefix(token, domain.OAuthRefreshTokenPrefix):
		_ = s.Repo.RevokeRefreshToken(ctx, HashToken(token))
	}
}

// AccessToken resolves a live OAuth access token.
func (s *Server) AccessToken(ctx context.Context, token string) (domain.OAuthAccessToken, bool) {
	if !strings.HasPrefix(token, domain.OAuthAccessTokenPrefix) {
		return domain.OAuthAccessToken{}, false
	}
	at, err := s.Repo.FindAccessToken(ctx, HashToken(token))
	if err != nil || !at.IsValidAt(s.now()) {
		return domain.OAuthAccessToken{}, false
	}
	return at, true
}

// newToken returns a random opaque token (prefix + 32 random bytes) and its
// hash for storage.
func newToken(prefix string) (plaintext string, hash []byte) {
	plaintext = prefix + randString(32)
	return plaintext, HashToken(plaintext)
}

// HashToken is the stored form of a token.
func HashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// AppendQuery adds the non-empty params to a URL, keeping its query.
func AppendQuery(rawURL string, params map[string]string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL // validated upstream
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
