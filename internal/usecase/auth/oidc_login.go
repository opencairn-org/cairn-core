// Package auth holds the user-facing authentication use cases: password login,
// OIDC login (this file), passkey/webauthn, PAT exchange.
//
// OIDC providers are configured entirely in the environment (CAIRN_OIDC_*) and
// passed into NewOIDCLogin as an in-memory slice — there is no client database
// and no admin-UI editing. The flow is a confidential authorization-code
// exchange (client secret); not a public client.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	oidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Errors visible to the HTTP layer. Each maps onto a distinct UI state.
var (
	ErrProviderNotFound    = errors.New("oidc provider not configured")
	ErrAutoProvisionDenied = errors.New("first-time login but provider has auto_provision=false")
	ErrSubjectMissing      = errors.New("id_token missing subject claim")
	ErrEmailMissing        = errors.New("id_token missing email claim required for auto-provision")
	ErrCodeExchangeFailed  = errors.New("oauth code exchange failed")
	ErrIDTokenVerifyFailed = errors.New("id_token verification failed")
)

// ErrClientDisabled is kept as an alias of ErrProviderNotFound so existing
// handler switches keep compiling; an unknown provider id is the only "not
// available" case now (env providers are always enabled).
var ErrClientDisabled = ErrProviderNotFound

// oidcUserStore finds users by email and creates them on first sign-in.
type oidcUserStore interface {
	port.UserReader
	port.UserLifecycleStore
}

// ---------------------------------------------------------------------------
// OIDCLogin — two-phase: StartLogin (build authorize URL) / CompleteLogin
// (verify the callback, find/create the user, write a session). Never touches
// HTTP — handlers translate cookies + query strings into these calls.
// ---------------------------------------------------------------------------

type OIDCLogin struct {
	providers       map[string]domain.OIDCProvider
	order           []string // config order, for Providers()
	Identities      port.LinkedIdentityRepo
	Sessions        port.SessionRepo
	Users           oidcUserStore
	SessionTTL      time.Duration
	RedirectBaseURL string // e.g. "https://cairn.example.com"; callback path appended

	now      func() time.Time
	newUUID  func() uuid.UUID
	verifier OIDCVerifierFactory
}

// OIDCVerifierFactory is the seam tests inject to bypass real IdP discovery.
type OIDCVerifierFactory func(ctx context.Context, issuerURL string) (OIDCVerifier, error)

// OIDCVerifier abstracts the subset of *oidc.Provider we use.
type OIDCVerifier interface {
	Endpoint() oauth2.Endpoint
	Verifier(cfg *oidc.Config) IDTokenVerifier
}

// IDTokenVerifier matches *oidc.IDTokenVerifier.
type IDTokenVerifier interface {
	Verify(ctx context.Context, rawIDToken string) (*oidc.IDToken, error)
}

// NewOIDCLogin wires the use case from the env-configured providers.
func NewOIDCLogin(
	providers []domain.OIDCProvider,
	identities port.LinkedIdentityRepo,
	sessions port.SessionRepo,
	users oidcUserStore,
	sessionTTL time.Duration,
	redirectBaseURL string,
) *OIDCLogin {
	m := make(map[string]domain.OIDCProvider, len(providers))
	order := make([]string, 0, len(providers))
	for _, p := range providers {
		m[p.ID] = p
		order = append(order, p.ID)
	}
	return &OIDCLogin{
		providers:       m,
		order:           order,
		Identities:      identities,
		Sessions:        sessions,
		Users:           users,
		SessionTTL:      sessionTTL,
		RedirectBaseURL: redirectBaseURL,
		now:             func() time.Time { return time.Now().UTC() },
		newUUID:         uuid.New,
		verifier:        defaultVerifierFactory,
	}
}

func (uc *OIDCLogin) SetClock(now func() time.Time)             { uc.now = now }
func (uc *OIDCLogin) SetUUID(fn func() uuid.UUID)               { uc.newUUID = fn }
func (uc *OIDCLogin) SetVerifierFactory(fn OIDCVerifierFactory) { uc.verifier = fn }

// Providers returns the configured providers in config order (for the login
// page buttons + the read-only admin view). Secrets are NOT included by the
// callers that serialise these.
func (uc *OIDCLogin) Providers() []domain.OIDCProvider {
	out := make([]domain.OIDCProvider, 0, len(uc.order))
	for _, id := range uc.order {
		out = append(out, uc.providers[id])
	}
	return out
}

func (uc *OIDCLogin) provider(id string) (domain.OIDCProvider, bool) {
	p, ok := uc.providers[id]
	return p, ok
}

// ---------------------------------------------------------------------------
// StartLogin
// ---------------------------------------------------------------------------

type StartLoginOutput struct {
	AuthURL      string
	State        string
	Nonce        string
	CodeVerifier string // empty when provider.UsePKCE = false
}

func (uc *OIDCLogin) StartLogin(ctx context.Context, providerID string) (StartLoginOutput, error) {
	p, ok := uc.provider(providerID)
	if !ok {
		return StartLoginOutput{}, ErrProviderNotFound
	}

	prov, err := uc.verifier(ctx, p.IssuerURL)
	if err != nil {
		return StartLoginOutput{}, fmt.Errorf("oidc provider discovery: %w", err)
	}

	state, err := randomURLSafe(24)
	if err != nil {
		return StartLoginOutput{}, err
	}
	nonce, err := randomURLSafe(24)
	if err != nil {
		return StartLoginOutput{}, err
	}

	cfg := uc.oauth2Config(p, prov.Endpoint())
	opts := []oauth2.AuthCodeOption{oidc.Nonce(nonce)}
	codeVerifier := ""
	if p.UsePKCE {
		codeVerifier, err = randomURLSafe(48)
		if err != nil {
			return StartLoginOutput{}, err
		}
		opts = append(opts,
			oauth2.SetAuthURLParam("code_challenge", s256(codeVerifier)),
			oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		)
	}

	return StartLoginOutput{
		AuthURL:      cfg.AuthCodeURL(state, opts...),
		State:        state,
		Nonce:        nonce,
		CodeVerifier: codeVerifier,
	}, nil
}

// ---------------------------------------------------------------------------
// CompleteLogin
// ---------------------------------------------------------------------------

type CompleteLoginInput struct {
	ProviderID    string
	Code          string
	CodeVerifier  string
	ExpectedNonce string
	ClientIP      string
	UserAgent     string
}

type CompleteLoginOutput struct {
	UserID      domain.UserID
	SessionID   domain.SessionID
	BearerToken string // plaintext — set as cookie value; never stored
	ExpiresAt   time.Time
	NewUser     bool
}

func (uc *OIDCLogin) CompleteLogin(ctx context.Context, in CompleteLoginInput) (CompleteLoginOutput, error) {
	p, ok := uc.provider(in.ProviderID)
	if !ok {
		return CompleteLoginOutput{}, ErrProviderNotFound
	}

	idToken, err := uc.oidcExchangeAndVerify(ctx, p, in)
	if err != nil {
		return CompleteLoginOutput{}, err
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return CompleteLoginOutput{}, fmt.Errorf("decode claims: %w", err)
	}

	subject := idToken.Subject
	if subject == "" {
		subject = stringClaim(claims, "sub")
	}
	if subject == "" {
		return CompleteLoginOutput{}, ErrSubjectMissing
	}

	linked, err := uc.Identities.FindBySubject(ctx, p.ID, subject)
	switch {
	case err == nil:
		_ = uc.Identities.TouchLastUsed(ctx, linked.ID, claims)
		return uc.issueSession(ctx, linked.UserID, in, false)

	case errors.Is(err, domain.ErrNotFound):
		return uc.oidcProvisionAndLink(ctx, p, in, subject, claims)

	default:
		return CompleteLoginOutput{}, fmt.Errorf("find linked identity: %w", err)
	}
}

// oidcExchangeAndVerify redeems the auth code and verifies the id_token incl. nonce.
func (uc *OIDCLogin) oidcExchangeAndVerify(ctx context.Context, p domain.OIDCProvider, in CompleteLoginInput) (*oidc.IDToken, error) {
	prov, err := uc.verifier(ctx, p.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc provider discovery: %w", err)
	}

	cfg := uc.oauth2Config(p, prov.Endpoint())
	exchangeOpts := []oauth2.AuthCodeOption{}
	if p.UsePKCE && in.CodeVerifier != "" {
		exchangeOpts = append(exchangeOpts, oauth2.SetAuthURLParam("code_verifier", in.CodeVerifier))
	}

	token, err := cfg.Exchange(ctx, in.Code, exchangeOpts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCodeExchangeFailed, err)
	}

	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		return nil, fmt.Errorf("%w: no id_token in response", ErrIDTokenVerifyFailed)
	}

	idTokenVerifier := prov.Verifier(&oidc.Config{
		ClientID:          p.EffectiveAudience(),
		SkipClientIDCheck: p.SkipAudienceCheck,
	})
	idToken, err := idTokenVerifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIDTokenVerifyFailed, err)
	}
	// The nonce is mandatory: StartLogin always sets one, so an empty expected
	// nonce here means a forged/replayed flow — fail closed rather than skip the
	// id-token replay check.
	if in.ExpectedNonce == "" || idToken.Nonce != in.ExpectedNonce {
		return nil, fmt.Errorf("%w: nonce mismatch", ErrIDTokenVerifyFailed)
	}
	return idToken, nil
}

// oidcProvisionAndLink handles a first login: resolve/create the user, link the identity, issue a session.
func (uc *OIDCLogin) oidcProvisionAndLink(ctx context.Context, p domain.OIDCProvider, in CompleteLoginInput, subject string, claims map[string]any) (CompleteLoginOutput, error) {
	if !p.AutoProvision {
		return CompleteLoginOutput{}, ErrAutoProvisionDenied
	}
	user, isNew, err := uc.resolveUserForNewIdentity(ctx, p, claims)
	if err != nil {
		return CompleteLoginOutput{}, err
	}
	if _, err := uc.Identities.Create(ctx, domain.LinkedIdentity{
		UserID:     user.ID,
		Provider:   p.ID,
		Subject:    subject,
		Email:      stringClaim(claims, domain.ClaimEmail),
		LastClaims: claims,
	}); err != nil {
		return CompleteLoginOutput{}, fmt.Errorf("link identity: %w", err)
	}
	return uc.issueSession(ctx, user.ID, in, isNew)
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

func (uc *OIDCLogin) oauth2Config(p domain.OIDCProvider, endpoint oauth2.Endpoint) *oauth2.Config {
	cfg := &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Scopes:       slices.Clone(p.Scopes),
		Endpoint:     endpoint,
		RedirectURL:  strings.TrimRight(uc.RedirectBaseURL, "/") + "/auth/oidc/callback",
	}
	if !slices.Contains(cfg.Scopes, "openid") {
		cfg.Scopes = append([]string{"openid"}, cfg.Scopes...)
	}
	return cfg
}

// resolveUserForNewIdentity attaches a brand-new identity to an existing user
// when its VERIFIED email matches one, else provisions a fresh user. Unverified
// emails never auto-link (anti-hijack). Returns (user, isNewUser, error).
func (uc *OIDCLogin) resolveUserForNewIdentity(ctx context.Context, p domain.OIDCProvider, claims map[string]any) (domain.User, bool, error) {
	email := stringClaim(claims, domain.ClaimEmail)
	if email != "" && boolClaim(claims, domain.ClaimEmailVerified) {
		existing, err := uc.Users.GetUserByEmail(ctx, strings.ToLower(email))
		switch {
		case err == nil:
			return existing, false, nil
		case !errors.Is(err, domain.ErrNotFound):
			return domain.User{}, false, fmt.Errorf("lookup user by email: %w", err)
		}
	}
	user, err := uc.provisionUser(ctx, p, claims)
	if err != nil {
		return domain.User{}, false, fmt.Errorf("provision user: %w", err)
	}
	return user, true, nil
}

// provisionUser creates a fresh Cairn user for a first-time OIDC identity.
//
// The username is derived from preferred_username (else the email local-part);
// because that can collide across IdPs/users, a numeric suffix is appended and
// retried on a username-unique violation. An email collision is NOT retried —
// it means an account already owns that address, and OIDC must never silently
// take it over (resolveUserForNewIdentity already handled the verified-email
// auto-link case).
//
// The new user's role comes from the provider's AUTO_PROVISION_ROLE. Note that
// admin grants instance admin on first login — only set it for a provider you
// fully trust to assert identity.
func (uc *OIDCLogin) provisionUser(ctx context.Context, p domain.OIDCProvider, claims map[string]any) (domain.User, error) {
	email := stringClaim(claims, domain.ClaimEmail)
	if email == "" {
		return domain.User{}, ErrEmailMissing
	}
	base := stringClaim(claims, domain.ClaimUsername)
	if base == "" {
		base = localPart(email)
	}
	displayName := stringClaim(claims, domain.ClaimDisplayName)
	if displayName == "" {
		displayName = base
	}
	role := p.AutoProvisionRole
	if role != domain.UserRoleAdmin {
		role = domain.UserRoleUser
	}

	const maxUsernameAttempts = 6
	for attempt := 0; attempt < maxUsernameAttempts; attempt++ {
		username := base
		if attempt > 0 {
			username = fmt.Sprintf("%s%d", base, attempt+1)
		}
		user := domain.User{
			ID:            domain.UserID(uc.newUUID()),
			Username:      username,
			Email:         email,
			EmailVerified: boolClaim(claims, domain.ClaimEmailVerified),
			DisplayName:   displayName,
			Role:          role,
			Status:        domain.UserStatusActive,
			CreatedAt:     uc.now(),
			UpdatedAt:     uc.now(),
		}
		// OIDC-provisioned users have no local password.
		err := uc.Users.CreateUser(ctx, user, domain.Credentials{UserID: user.ID})
		switch {
		case err == nil:
			return user, nil
		case errors.Is(err, port.ErrUsernameTaken):
			continue // collision — try the next numeric suffix
		case errors.Is(err, port.ErrEmailTaken):
			return domain.User{}, fmt.Errorf("an account already exists for this email address: %w", err)
		default:
			return domain.User{}, err
		}
	}
	return domain.User{}, fmt.Errorf("could not allocate a unique username from %q: %w", base, port.ErrUsernameTaken)
}

func (uc *OIDCLogin) issueSession(ctx context.Context, userID domain.UserID, in CompleteLoginInput, newUser bool) (CompleteLoginOutput, error) {
	plaintext, err := randomURLSafe(32)
	if err != nil {
		return CompleteLoginOutput{}, err
	}
	hash := sha256.Sum256([]byte(plaintext))
	expiresAt := uc.now().Add(uc.SessionTTL)
	sid, err := uc.Sessions.Create(ctx, domain.Session{
		UserID:           userID,
		TokenHash:        hash[:],
		AuthMethod:       domain.SessionAuthOIDC,
		UserAgentSummary: truncate(in.UserAgent, 200),
		CreatedAt:        uc.now(),
		LastSeenAt:       uc.now(),
		ExpiresAt:        expiresAt,
	})
	if err != nil {
		return CompleteLoginOutput{}, fmt.Errorf("create session: %w", err)
	}
	return CompleteLoginOutput{
		UserID: userID, SessionID: sid, BearerToken: plaintext, ExpiresAt: expiresAt, NewUser: newUser,
	}, nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func randomURLSafe(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func s256(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func stringClaim(claims map[string]any, key string) string {
	if key == "" {
		return ""
	}
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}

func boolClaim(claims map[string]any, key string) bool {
	if v, ok := claims[key].(bool); ok {
		return v
	}
	return false
}

func localPart(email string) string {
	if idx := strings.IndexByte(email, '@'); idx > 0 {
		return email[:idx]
	}
	return email
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// defaultVerifierFactory wraps coreos/go-oidc's real Provider.
func defaultVerifierFactory(ctx context.Context, issuerURL string) (OIDCVerifier, error) {
	p, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, err
	}
	return &realVerifier{p}, nil
}

type realVerifier struct{ p *oidc.Provider }

func (r *realVerifier) Endpoint() oauth2.Endpoint                 { return r.p.Endpoint() }
func (r *realVerifier) Verifier(cfg *oidc.Config) IDTokenVerifier { return r.p.Verifier(cfg) }
