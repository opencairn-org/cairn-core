package rest

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
	"github.com/opencairn-org/cairn-core/internal/usecase/oauthserver"
)

// OAuthServerHandlers serve the OAuth 2.1 authorization server: Cairn issuing
// scoped tokens to native apps, third-party clients and the MCP server (not
// to be confused with linking a provider account, see ConnectionHandlers).
//
//	GET  /.well-known/oauth-authorization-server   AS metadata (RFC 8414)
//	GET  /.well-known/oauth-protected-resource     resource metadata (RFC 9728)
//	GET  /oauth/authorize            validate, then hand off to the SPA consent page
//	GET  /oauth/authorize/info       consent-page data (client name + scopes)
//	POST /oauth/authorize/decision   session-authed approve/deny → mints the code
//	POST /oauth/token                code, refresh and device_code grants
//	POST /oauth/revoke               token revocation (RFC 7009)
//	POST /oauth/register             dynamic client registration (RFC 7591), opt-in
//	POST /oauth/device_authorization device asks for a code pair (RFC 8628)
//	GET  /oauth/device/info          session-authed: who is asking, for what
//	POST /oauth/device/decision      session-authed approve/deny
type OAuthServerHandlers struct {
	Auth    *Auth
	Server  *oauthserver.Server
	Devices *account.DeviceAuthorizations
	Log     *slog.Logger
	// BaseURL is the issuer, without a trailing slash.
	BaseURL string
	// DynamicRegistration enables POST /oauth/register.
	DynamicRegistration bool
	// DeviceLimit reports whether a client address may hit the device
	// endpoints again; nil means unlimited.
	DeviceLimit func(ip string) bool
}

// oauthDeviceVerifyPath is the page behind verification_uri; the SPA serves it.
const oauthDeviceVerifyPath = "/oauth/device"

// Mount registers the routes.
func (h *OAuthServerHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", h.metadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", h.protectedResourceMetadata)
	mux.HandleFunc("GET /oauth/authorize", h.authorize)
	mux.HandleFunc("GET /oauth/authorize/info", h.authorizeInfo)
	mux.HandleFunc("POST /oauth/authorize/decision", h.decision)
	mux.HandleFunc("POST /oauth/token", h.token)
	mux.HandleFunc("POST /oauth/revoke", h.revoke)
	// Guessing user codes and filling the table get a per-address limit.
	mux.HandleFunc("POST /oauth/device_authorization", h.limited(h.deviceAuthorization))
	mux.HandleFunc("GET /oauth/device/info", h.limited(h.deviceInfo))
	mux.HandleFunc("POST /oauth/device/decision", h.limited(h.deviceDecision))
	if h.DynamicRegistration {
		mux.HandleFunc("POST /oauth/register", h.register)
	}
}

func (h *OAuthServerHandlers) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.DeviceLimit != nil && !h.DeviceLimit(clientIP(r)) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

func (h *OAuthServerHandlers) metadata(w http.ResponseWriter, _ *http.Request) {
	md := map[string]any{
		"issuer":                                h.BaseURL,
		"authorization_endpoint":                h.BaseURL + "/oauth/authorize",
		"token_endpoint":                        h.BaseURL + "/oauth/token",
		"revocation_endpoint":                   h.BaseURL + "/oauth/revoke",
		"device_authorization_endpoint":         h.BaseURL + "/oauth/device_authorization",
		"scopes_supported":                      domain.SupportedScopes,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", domain.GrantTypeDeviceCode},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
	}
	if h.DynamicRegistration {
		md["registration_endpoint"] = h.BaseURL + "/oauth/register"
	}
	oauthWriteCachedJSON(w, md)
}

func (h *OAuthServerHandlers) protectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	oauthWriteCachedJSON(w, map[string]any{
		"resource":                 h.BaseURL,
		"authorization_servers":    []string{h.BaseURL},
		"scopes_supported":         domain.SupportedScopes,
		"bearer_methods_supported": []string{"header"},
	})
}

// authorize validates the request enough to trust the redirect_uri, then
// hands off to the SPA consent page. Bad client_id/redirect_uri are shown
// directly; everything else is reported to the client via a redirect.
func (h *OAuthServerHandlers) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirectURI, err := h.Server.Authorize(r.Context(), oauthserver.AuthorizeRequest{
		ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), ResponseType: q.Get("response_type"),
		CodeChallenge: q.Get("code_challenge"), CodeChallengeMethod: q.Get("code_challenge_method"),
	})
	var oe *oauthserver.Error
	switch {
	case errors.As(err, &oe):
		http.Redirect(w, r, oauthserver.AppendQuery(redirectURI, map[string]string{ //nolint:gosec // G710: only a registered redirect_uri of the client
			"error": oe.Code, "error_description": oe.Description, "state": q.Get("state"),
		}), http.StatusFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Redirect(w, r, "/oauth/consent?"+r.URL.RawQuery, http.StatusFound) //nolint:gosec // G710: fixed same-origin path; the query cannot change the target
	}
}

// authorizeInfo returns the data the consent page renders.
func (h *OAuthServerHandlers) authorizeInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.Auth.requireUser(w, r); !ok {
		return
	}
	q := r.URL.Query()
	client, scopes, err := h.Server.Consent(r.Context(), q.Get("client_id"), q.Get("scope"))
	if err != nil {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client_name": client.Name,
		"client_id":   client.ClientID,
		"scopes":      oauthScopeInfos(scopes),
	})
}

// decision is the session-authed approve/deny; it returns the redirect URL
// for the SPA to navigate to.
func (h *OAuthServerHandlers) decision(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	var req struct {
		ClientID            string `json:"client_id"`
		RedirectURI         string `json:"redirect_uri"`
		Scope               string `json:"scope"`
		State               string `json:"state"`
		CodeChallenge       string `json:"code_challenge"`
		CodeChallengeMethod string `json:"code_challenge_method"`
		Approve             bool   `json:"approve"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	redirect, err := h.Server.Decide(r.Context(), userID, oauthserver.Decision(req))
	switch {
	case errors.Is(err, oauthserver.ErrUnknownClient), errors.Is(err, oauthserver.ErrInvalidRedirect):
		http.Error(w, "invalid client/redirect", http.StatusBadRequest)
	case errors.Is(err, oauthserver.ErrMissingChallenge):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case err != nil:
		h.Log.Error("oauth: create auth code", "error", err)
		http.Error(w, "server error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"redirect_uri": redirect})
	}
}

func (h *OAuthServerHandlers) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthTokenError(w, "invalid_request", "malformed form")
		return
	}
	creds := oauthClientCredentials(r)
	var (
		grant oauthserver.TokenGrant
		err   error
	)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		grant, err = h.Server.ExchangeCode(r.Context(), creds,
			r.PostForm.Get("code"), r.PostForm.Get("redirect_uri"), r.PostForm.Get("code_verifier"))
	case "refresh_token":
		grant, err = h.Server.Refresh(r.Context(), creds, r.PostForm.Get("refresh_token"), r.PostForm.Get("scope"))
	case domain.GrantTypeDeviceCode:
		grant, err = h.Server.PollDevice(r.Context(), creds, r.PostForm.Get("device_code"))
	default:
		oauthTokenError(w, "unsupported_grant_type", "grant_type must be authorization_code, refresh_token or device_code")
		return
	}
	if err != nil {
		h.writeProtocolError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  grant.AccessToken,
		"token_type":    "Bearer",
		"expires_in":    int(grant.ExpiresIn.Seconds()),
		"refresh_token": grant.RefreshToken,
		"scope":         grant.Scope,
	})
}

func (h *OAuthServerHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err == nil {
		h.Server.Revoke(r.Context(), r.PostForm.Get("token"))
	}
	w.WriteHeader(http.StatusOK) // RFC 7009: always 200
}

func (h *OAuthServerHandlers) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		Scope                   string   `json:"scope"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		SoftwareID              string   `json:"software_id"`
		SoftwareVersion         string   `json:"software_version"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		oauthTokenError(w, "invalid_client_metadata", "malformed body")
		return
	}
	reg, err := h.Server.Register(r.Context(), oauthserver.Registration(req))
	if err != nil {
		h.writeProtocolError(w, err)
		return
	}
	c := reg.Client
	resp := map[string]any{
		"client_id":                  c.ClientID,
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                c.GrantTypes,
		"scope":                      domain.JoinScopes(c.Scopes),
		"token_endpoint_auth_method": c.TokenEndpointAuthMethod,
	}
	if c.SoftwareID != "" {
		resp["software_id"] = c.SoftwareID
	}
	if c.SoftwareVersion != "" {
		resp["software_version"] = c.SoftwareVersion
	}
	if reg.Secret != "" {
		resp["client_secret"] = reg.Secret
	}
	writeJSON(w, http.StatusCreated, resp)
}

// deviceAuthorization issues a code pair to a public or authenticated client.
func (h *OAuthServerHandlers) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthTokenError(w, "invalid_request", "malformed form")
		return
	}
	grant, err := h.Server.RequestDevice(r.Context(), oauthClientCredentials(r), r.PostForm.Get("scope"))
	if err != nil {
		h.writeProtocolError(w, err)
		return
	}
	verify := h.BaseURL + oauthDeviceVerifyPath
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               grant.DeviceCode,
		"user_code":                 grant.UserCode,
		"verification_uri":          verify,
		"verification_uri_complete": verify + "?user_code=" + grant.UserCode,
		"expires_in":                int(grant.ExpiresIn.Seconds()),
		"interval":                  int(grant.Interval.Seconds()),
	})
}

// deviceInfo is what the verification page shows before the person decides.
// Approving from the browser needs a session or a PAT, never an OAuth token.
func (h *OAuthServerHandlers) deviceInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.Auth.UserNoOAuth(r); !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	pending, err := h.Devices.Lookup(r.Context(), r.URL.Query().Get("user_code"), nil)
	if err != nil {
		oauthDeviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client_name":      pending.Client.Name,
		"client_id":        pending.Client.ClientID,
		"software_id":      pending.Client.SoftwareID,
		"software_version": pending.Client.SoftwareVersion,
		"scopes":           oauthScopeInfos(pending.Scopes),
		"expires_at":       pending.Code.ExpiresAt,
	})
}

// deviceDecision records approve/deny from the verification page.
func (h *OAuthServerHandlers) deviceDecision(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.UserNoOAuth(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var req struct {
		UserCode string `json:"user_code"`
		Approve  bool   `json:"approve"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	granted, err := h.Devices.Decide(r.Context(), userID, req.UserCode, req.Approve, nil)
	if err != nil {
		oauthDeviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approved": req.Approve, "granted_scopes": granted})
}

// writeProtocolError renders a use-case error as an RFC 6749 error body.
func (h *OAuthServerHandlers) writeProtocolError(w http.ResponseWriter, err error) {
	var oe *oauthserver.Error
	if !errors.As(err, &oe) {
		oe = &oauthserver.Error{Code: "server_error", Description: "server error", Err: err}
	}
	if oe.Err != nil {
		h.Log.Error("oauth: "+oe.Description, "error", oe.Err)
	}
	oauthTokenError(w, oe.Code, oe.Description)
}

// oauthClientCredentials reads the client from HTTP Basic, else the form.
func oauthClientCredentials(r *http.Request) oauthserver.ClientCredentials {
	if id, sec, ok := r.BasicAuth(); ok {
		return oauthserver.ClientCredentials{ID: id, Secret: sec}
	}
	return oauthserver.ClientCredentials{ID: r.PostForm.Get("client_id"), Secret: r.PostForm.Get("client_secret")}
}

type oauthScopeInfo struct {
	Scope       string `json:"scope"`
	Description string `json:"description"`
	Write       bool   `json:"write"`
}

func oauthScopeInfos(scopes []string) []oauthScopeInfo {
	infos := make([]oauthScopeInfo, 0, len(scopes))
	for _, sc := range scopes {
		infos = append(infos, oauthScopeInfo{Scope: sc, Description: domain.ScopeDescription(sc), Write: domain.IsWriteScope(sc)})
	}
	return infos
}

func oauthWriteCachedJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, v)
}

func oauthTokenError(w http.ResponseWriter, code, desc string) {
	status := http.StatusBadRequest
	switch code {
	case "invalid_client":
		status = http.StatusUnauthorized
	case "server_error":
		status = http.StatusInternalServerError
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

func oauthDeviceError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if ae, ok := actionErrorOf(err); ok {
		switch ae.Kind {
		case port.ActionErrorInvalid:
			status = http.StatusBadRequest
		case port.ActionErrorNotFound:
			status = http.StatusNotFound
		}
	}
	http.Error(w, err.Error(), status)
}
