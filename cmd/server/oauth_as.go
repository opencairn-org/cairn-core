package main

// OAuth 2.1 authorization server.
//
// This is the *authorization server* side of Cairn — distinct from
// oauth_connect.go (which links a user's *provider* account, e.g. Strava).
// Here Cairn issues scoped, revocable tokens to native apps, third-party
// clients and the MCP server via authorization-code + PKCE, with RFC 7591
// dynamic client registration and RFC 8414 / RFC 9728 discovery.
//
// Flow:
//   GET  /oauth/authorize         validate, then hand off to the SPA consent page
//   GET  /oauth/authorize/info    consent-page data (client name + scopes)
//   POST /oauth/authorize/decision  session-authed approve/deny → mints the code
//   POST /oauth/token             code→token (PKCE) and refresh-token rotation
//   POST /oauth/register          dynamic client registration
//   POST /oauth/revoke            token revocation
//   /oauth/device*                device authorization grant (oauth_device.go)
//   GET  /.well-known/oauth-authorization-server   AS metadata (RFC 8414)
//   GET  /.well-known/oauth-protected-resource     resource metadata (RFC 9728)

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
)

const (
	oauthAccessTokenPrefix  = domain.OAuthAccessTokenPrefix
	oauthRefreshTokenPrefix = domain.OAuthRefreshTokenPrefix
	oauthAuthCodePrefix     = domain.OAuthAuthCodePrefix
)

// mountOAuthServer wires the OAuth 2.1 authorization-server endpoints. No-op
// (caller-gated) unless app.OAuth is set.
func mountOAuthServer(mux *http.ServeMux, app *App, logger *slog.Logger, baseURL string, auth config.AuthConfig) {
	if app.OAuth == nil {
		return
	}
	s := &oauthServer{
		app: app, oauth: app.OAuth, devices: app.DeviceAuthorizations,
		log: logger, baseURL: strings.TrimRight(baseURL, "/"), auth: auth,
	}

	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.metadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResourceMetadata)
	mux.HandleFunc("GET /oauth/authorize", s.authorize)
	mux.HandleFunc("GET /oauth/authorize/info", s.authorizeInfo)
	mux.HandleFunc("POST /oauth/authorize/decision", s.decision)
	mux.HandleFunc("POST /oauth/token", s.token)
	mux.HandleFunc("POST /oauth/revoke", s.revoke)
	s.mountDeviceAuthorization(mux)
	if auth.OAuthAllowDynamicReg {
		mux.HandleFunc("POST /oauth/register", s.register)
	}
	logger.Info("oauth authorization server mounted",
		"dynamic_registration", auth.OAuthAllowDynamicReg, "issuer", s.baseURL)
}

type oauthServer struct {
	app *App
	// oauth and devices are what the handlers use; split from app so tests can
	// run them on a fake repository.
	oauth   port.OAuthServerRepo
	devices *account.DeviceAuthorizations
	log     *slog.Logger
	baseURL string
	auth    config.AuthConfig
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

func (s *oauthServer) metadata(w http.ResponseWriter, _ *http.Request) {
	md := map[string]any{
		"issuer":                                s.baseURL,
		"authorization_endpoint":                s.baseURL + "/oauth/authorize",
		"token_endpoint":                        s.baseURL + "/oauth/token",
		"revocation_endpoint":                   s.baseURL + "/oauth/revoke",
		"device_authorization_endpoint":         s.baseURL + "/oauth/device_authorization",
		"scopes_supported":                      domain.SupportedScopes,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", domain.GrantTypeDeviceCode},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
	}
	if s.auth.OAuthAllowDynamicReg {
		md["registration_endpoint"] = s.baseURL + "/oauth/register"
	}
	writeJSONCache(w, md)
}

func (s *oauthServer) protectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSONCache(w, map[string]any{
		"resource":                 s.baseURL,
		"authorization_servers":    []string{s.baseURL},
		"scopes_supported":         domain.SupportedScopes,
		"bearer_methods_supported": []string{"header"},
	})
}

// ---------------------------------------------------------------------------
// Authorization endpoint
// ---------------------------------------------------------------------------

// authorize validates the request enough to trust the redirect_uri, then hands
// off to the SPA consent page (which handles login + scope approval). Hard
// errors (bad client_id / redirect_uri) are shown directly; everything else is
// reported to the client via a redirect.
func (s *oauthServer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")

	client, err := s.oauth.GetClient(r.Context(), clientID)
	if err != nil {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	redirectURI, ok := client.RegisteredRedirectURI(q.Get("redirect_uri"))
	if !ok {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	// From here, errors are safe to redirect back to the client.
	state := q.Get("state")
	if q.Get("response_type") != "code" {
		s.redirectErr(w, r, redirectURI, state, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	if !client.AllowsGrant("authorization_code") {
		s.redirectErr(w, r, redirectURI, state, "unauthorized_client", "client may not use the authorization_code grant")
		return
	}
	if q.Get("code_challenge") == "" {
		s.redirectErr(w, r, redirectURI, state, "invalid_request", "PKCE code_challenge is required")
		return
	}
	if m := q.Get("code_challenge_method"); m != "" && m != "S256" {
		s.redirectErr(w, r, redirectURI, state, "invalid_request", "only S256 code_challenge_method is supported")
		return
	}
	// Hand off to the SPA consent page, preserving the request verbatim.
	http.Redirect(w, r, "/oauth/consent?"+r.URL.RawQuery, http.StatusFound) //nolint:gosec // G710: fixed same-origin path; the query cannot change the target
}

// authorizeInfo returns the data the consent page renders. Session-authed: the
// user must be logged in to see consent details.
func (s *oauthServer) authorizeInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := resolveSessionUser(r, s.app); !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	client, err := s.oauth.GetClient(r.Context(), q.Get("client_id"))
	if err != nil {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	scopes := domain.FilterRequestedScopes(domain.ParseScopes(q.Get("scope")), client.Scopes)
	oauthJSON(w, map[string]any{
		"client_name": client.Name,
		"client_id":   client.ClientID,
		"scopes":      scopeInfos(scopes),
	})
}

// decisionRequest is the JSON the consent page POSTs.
type decisionRequest struct {
	ClientID            string `json:"client_id"`
	RedirectURI         string `json:"redirect_uri"`
	Scope               string `json:"scope"`
	State               string `json:"state"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
	Approve             bool   `json:"approve"`
}

// decision is the session-authed approve/deny. On approve it mints a one-time
// authorization code and returns the redirect URL for the SPA to navigate to.
func (s *oauthServer) decision(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, s.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var req decisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	client, err := s.oauth.GetClient(r.Context(), req.ClientID)
	if err != nil {
		http.Error(w, "invalid client/redirect", http.StatusBadRequest)
		return
	}
	redirectURI, ok := client.RegisteredRedirectURI(req.RedirectURI)
	if !ok {
		http.Error(w, "invalid client/redirect", http.StatusBadRequest)
		return
	}
	if !req.Approve {
		oauthJSON(w, map[string]string{"redirect_uri": appendQuery(redirectURI, map[string]string{
			"error": "access_denied", "state": req.State,
		})})
		return
	}
	if req.CodeChallenge == "" {
		http.Error(w, "missing code_challenge", http.StatusBadRequest)
		return
	}
	scopes := domain.FilterRequestedScopes(domain.ParseScopes(req.Scope), client.Scopes)

	code, codeHash := newToken(oauthAuthCodePrefix)
	method := req.CodeChallengeMethod
	if method == "" {
		method = "S256"
	}
	now := time.Now().UTC()
	err = s.oauth.CreateAuthCode(r.Context(), domain.OAuthAuthorizationCode{
		ClientID:            client.ClientID,
		UserID:              userID,
		RedirectURI:         redirectURI,
		Scope:               domain.JoinScopes(scopes),
		CodeChallenge:       req.CodeChallenge,
		CodeChallengeMethod: method,
		ExpiresAt:           now.Add(s.auth.OAuthAuthCodeTTL),
	}, codeHash)
	if err != nil {
		s.log.Error("oauth: create auth code", "error", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	oauthJSON(w, map[string]string{"redirect_uri": appendQuery(redirectURI, map[string]string{
		"code": code, "state": req.State,
	})})
}

// ---------------------------------------------------------------------------
// Token endpoint
// ---------------------------------------------------------------------------

func (s *oauthServer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthTokenErr(w, "invalid_request", "malformed form")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.tokenAuthCode(w, r)
	case "refresh_token":
		s.tokenRefresh(w, r)
	case domain.GrantTypeDeviceCode:
		s.tokenDeviceCode(w, r)
	default:
		oauthTokenErr(w, "unsupported_grant_type", "grant_type must be authorization_code, refresh_token or device_code")
	}
}

func (s *oauthServer) tokenAuthCode(w http.ResponseWriter, r *http.Request) {
	clientID, ok := s.authClient(r)
	if !ok {
		oauthTokenErr(w, "invalid_client", "client authentication failed")
		return
	}
	codePlain := r.PostForm.Get("code")
	if !strings.HasPrefix(codePlain, oauthAuthCodePrefix) {
		oauthTokenErr(w, "invalid_grant", "invalid code")
		return
	}
	codeHash := hashToken(codePlain)
	code, err := s.oauth.ConsumeAuthCode(r.Context(), codeHash, time.Now().UTC())
	if err != nil {
		oauthTokenErr(w, "invalid_grant", "code invalid, expired, or already used")
		return
	}
	if code.ClientID != clientID {
		oauthTokenErr(w, "invalid_grant", "code was issued to a different client")
		return
	}
	if code.RedirectURI != r.PostForm.Get("redirect_uri") {
		oauthTokenErr(w, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if !domain.VerifyPKCE(r.PostForm.Get("code_verifier"), code.CodeChallenge, code.CodeChallengeMethod) {
		oauthTokenErr(w, "invalid_grant", "PKCE verification failed")
		return
	}
	s.issueTokens(w, r, clientID, code.UserID, code.Scope)
}

func (s *oauthServer) tokenRefresh(w http.ResponseWriter, r *http.Request) {
	clientID, ok := s.authClient(r)
	if !ok {
		oauthTokenErr(w, "invalid_client", "client authentication failed")
		return
	}
	rt := r.PostForm.Get("refresh_token")
	if !strings.HasPrefix(rt, oauthRefreshTokenPrefix) {
		oauthTokenErr(w, "invalid_grant", "invalid refresh_token")
		return
	}
	hash := hashToken(rt)
	stored, err := s.oauth.FindRefreshToken(r.Context(), hash)
	if err != nil || !stored.IsValidAt(time.Now().UTC()) || stored.ClientID != clientID {
		oauthTokenErr(w, "invalid_grant", "refresh_token invalid or expired")
		return
	}
	// Rotate: revoke the presented refresh token, issue a fresh pair.
	_ = s.oauth.RevokeRefreshToken(r.Context(), hash)
	// Allow narrowing scope on refresh, never widening.
	scope := stored.Scope
	if req := r.PostForm.Get("scope"); req != "" {
		narrowed := domain.FilterRequestedScopes(domain.ParseScopes(req), domain.ParseScopes(stored.Scope))
		scope = domain.JoinScopes(narrowed)
	}
	s.issueTokens(w, r, clientID, stored.UserID, scope)
}

// issueTokens mints + persists an access/refresh pair and writes the token
// response.
func (s *oauthServer) issueTokens(w http.ResponseWriter, r *http.Request, clientID string, userID domain.UserID, scope string) {
	now := time.Now().UTC()
	at, atHash := newToken(oauthAccessTokenPrefix)
	rt, rtHash := newToken(oauthRefreshTokenPrefix)
	atExp := now.Add(s.auth.OAuthAccessTokenTTL)

	if err := s.oauth.CreateAccessToken(r.Context(), domain.OAuthAccessToken{
		ClientID: clientID, UserID: userID, Scope: scope, ExpiresAt: atExp,
	}, atHash); err != nil {
		s.log.Error("oauth: create access token", "error", err)
		oauthTokenErr(w, "server_error", "could not issue token")
		return
	}
	if err := s.oauth.CreateRefreshToken(r.Context(), domain.OAuthRefreshToken{
		ClientID: clientID, UserID: userID, Scope: scope, ExpiresAt: now.Add(s.auth.OAuthRefreshTokenTTL),
	}, rtHash); err != nil {
		s.log.Error("oauth: create refresh token", "error", err)
		oauthTokenErr(w, "server_error", "could not issue token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	oauthJSON(w, map[string]any{
		"access_token":  at,
		"token_type":    "Bearer",
		"expires_in":    int(s.auth.OAuthAccessTokenTTL.Seconds()),
		"refresh_token": rt,
		"scope":         scope,
	})
}

// ---------------------------------------------------------------------------
// Revocation
// ---------------------------------------------------------------------------

func (s *oauthServer) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusOK) // RFC 7009: always 200
		return
	}
	tok := r.PostForm.Get("token")
	switch {
	case strings.HasPrefix(tok, oauthAccessTokenPrefix):
		_ = s.oauth.RevokeAccessToken(r.Context(), hashToken(tok))
	case strings.HasPrefix(tok, oauthRefreshTokenPrefix):
		_ = s.oauth.RevokeRefreshToken(r.Context(), hashToken(tok))
	}
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// Dynamic client registration (RFC 7591)
// ---------------------------------------------------------------------------

type registrationRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	Scope                   string   `json:"scope"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	// SoftwareID / SoftwareVersion (RFC 7591 §2) name the application; stored
	// verbatim (capped) so the user can recognise the app later.
	SoftwareID      string `json:"software_id"`
	SoftwareVersion string `json:"software_version"`
}

// registrationFieldMax caps free-text registration metadata.
const registrationFieldMax = 200

// clipField trims s and caps it at registrationFieldMax runes.
func clipField(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > registrationFieldMax {
		return string(r[:registrationFieldMax])
	}
	return s
}

// withDefaults fills unset registration fields.
func (req registrationRequest) withDefaults() (authMethod string, grants, scopes []string, name string) {
	authMethod = req.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none" // default to a public (PKCE) client
	}
	grants = req.GrantTypes
	if len(grants) == 0 {
		grants = []string{"authorization_code", "refresh_token", domain.GrantTypeDeviceCode}
	}
	// Default to read-only scopes; intersect any requested with the catalog.
	scopes = domain.ReadOnlyScopes
	if req.Scope != "" {
		scopes = domain.FilterRequestedScopes(domain.ParseScopes(req.Scope), domain.SupportedScopes)
	}
	name = clipField(req.ClientName)
	if name == "" {
		name = "Dynamic client"
	}
	return authMethod, grants, scopes, name
}

func (s *oauthServer) register(w http.ResponseWriter, r *http.Request) {
	var req registrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oauthTokenErr(w, "invalid_client_metadata", "malformed body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthTokenErr(w, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if _, err := url.Parse(u); err != nil {
			oauthTokenErr(w, "invalid_redirect_uri", "redirect_uri is not a valid URL")
			return
		}
	}
	authMethod, grants, scopes, name := req.withDefaults()

	clientID := "cairn_dcr_" + randString(16)
	client := domain.OAuthClient{
		ClientID:                clientID,
		Name:                    name,
		RedirectURIs:            req.RedirectURIs,
		GrantTypes:              grants,
		Scopes:                  scopes,
		TokenEndpointAuthMethod: authMethod,
		IsDynamic:               true,
		SoftwareID:              clipField(req.SoftwareID),
		SoftwareVersion:         clipField(req.SoftwareVersion),
	}
	resp := map[string]any{
		"client_id":                  clientID,
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                grants,
		"scope":                      domain.JoinScopes(scopes),
		"token_endpoint_auth_method": authMethod,
	}
	if client.SoftwareID != "" {
		resp["software_id"] = client.SoftwareID
	}
	if client.SoftwareVersion != "" {
		resp["software_version"] = client.SoftwareVersion
	}
	if authMethod != "none" {
		secret := randString(40)
		h := sha256.Sum256([]byte(secret))
		client.SecretHash = h[:]
		resp["client_secret"] = secret
	}
	if err := s.oauth.CreateClient(r.Context(), client); err != nil {
		s.log.Error("oauth: register client", "error", err)
		oauthTokenErr(w, "server_error", "could not register client")
		return
	}
	w.WriteHeader(http.StatusCreated)
	oauthJSON(w, resp)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// authClient resolves + authenticates the calling client from the token
// request. Public clients send client_id in the body (no secret); confidential
// clients use HTTP Basic or client_secret_post. Returns (clientID, ok).
func (s *oauthServer) authClient(r *http.Request) (string, bool) {
	clientID := r.PostForm.Get("client_id")
	secret := r.PostForm.Get("client_secret")
	if id, sec, ok := r.BasicAuth(); ok {
		clientID, secret = id, sec
	}
	if clientID == "" {
		return "", false
	}
	client, err := s.oauth.GetClient(r.Context(), clientID)
	if err != nil {
		return "", false
	}
	if client.IsPublic() {
		return clientID, true // PKCE provides the proof; no secret expected
	}
	if !client.VerifySecret(secret) {
		return "", false
	}
	return clientID, true
}

func (s *oauthServer) redirectErr(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	http.Redirect(w, r, appendQuery(redirectURI, map[string]string{ //nolint:gosec // G710: callers pass only a registered redirect_uri of the client
		"error": code, "error_description": desc, "state": state,
	}), http.StatusFound)
}

// newToken returns a random opaque token (prefix + 32 random bytes, base64url)
// and its SHA-256 hash for storage.
func newToken(prefix string) (plaintext string, hash []byte) {
	plaintext = prefix + randString(32)
	return plaintext, hashToken(plaintext)
}

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// appendQuery adds non-empty params to a URL, preserving any existing query.
func appendQuery(rawURL string, params map[string]string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		// Fall back to naive join; redirect_uri was validated upstream.
		return rawURL
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

func scopeDescription(scope string) string { return domain.ScopeDescription(scope) }

func oauthJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONCache(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = json.NewEncoder(w).Encode(v)
}

func oauthTokenErr(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	status := http.StatusBadRequest
	switch code {
	case "invalid_client":
		status = http.StatusUnauthorized
	case "server_error":
		status = http.StatusInternalServerError
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}
