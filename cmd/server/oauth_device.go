package main

// OAuth 2.0 device authorization grant (RFC 8628) on top of oauth_as.go.
//
//   POST /oauth/device_authorization   device asks for a code pair (client_id, scope)
//   POST /oauth/token                  grant_type=urn:ietf:params:oauth:grant-type:device_code (polling)
//   GET  /oauth/device                 the SPA verification page (verification_uri)
//   GET  /oauth/device/info            session-authed: who is asking, for what
//   POST /oauth/device/decision        session-authed approve/deny
//
// The device shows user_code (and a QR of verification_uri_complete); the
// person approves on a client that is already signed in: this page in the
// browser, or the AuthService Lookup/DecideDeviceAuthorization RPCs in the app.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

const (
	oauthDeviceCodePrefix = "cairn_dc_"
	// deviceVerifyPath is the page behind verification_uri; the SPA serves it.
	deviceVerifyPath = "/oauth/device"
	// userCodeAttempts bounds the retry on a user-code collision.
	userCodeAttempts = 5
)

// mountDeviceAuthorization adds the RFC 8628 endpoints. The verification
// endpoints (guessing a user code) and the request endpoint (filling the table)
// get a per-address limiter on top of the global one.
func (s *oauthServer) mountDeviceAuthorization(mux *http.ServeMux) {
	limiter := newIPRateLimiter(deviceRequestsPerMinute, time.Now)
	limited := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !limiter.allow(clientIP(r)) {
				w.Header().Set("Retry-After", "10")
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /oauth/device_authorization", limited(s.deviceAuthorization))
	mux.HandleFunc("GET /oauth/device/info", limited(s.deviceInfo))
	mux.HandleFunc("POST /oauth/device/decision", limited(s.deviceDecision))
}

const deviceRequestsPerMinute = 20

// deviceAuthorization issues a code pair to a public or authenticated client.
func (s *oauthServer) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthTokenErr(w, "invalid_request", "malformed form")
		return
	}
	clientID, ok := s.authClient(r)
	if !ok {
		oauthTokenErr(w, "invalid_client", "client authentication failed")
		return
	}
	client, err := s.oauth.GetClient(r.Context(), clientID)
	if err != nil {
		oauthTokenErr(w, "invalid_client", "unknown client")
		return
	}
	if !client.AllowsGrant(domain.GrantTypeDeviceCode) {
		oauthTokenErr(w, "unauthorized_client", "client may not use the device_code grant")
		return
	}
	scopes := domain.FilterRequestedScopes(domain.ParseScopes(r.PostForm.Get("scope")), client.Scopes)
	now := time.Now().UTC()
	deviceCode, hash := newToken(oauthDeviceCodePrefix)
	var userCode string
	for attempt := 0; ; attempt++ {
		userCode, err = domain.NewUserCode()
		if err != nil {
			break
		}
		err = s.oauth.CreateDeviceCode(r.Context(), domain.OAuthDeviceCode{
			ClientID: client.ClientID, UserCode: userCode, Scope: domain.JoinScopes(scopes),
			ExpiresAt: now.Add(s.auth.OAuthDeviceCodeTTL),
		}, hash)
		if !errors.Is(err, domain.ErrUnique) || attempt+1 >= userCodeAttempts {
			break
		}
	}
	if err != nil {
		s.log.Error("oauth: create device code", "error", err)
		oauthTokenErr(w, "server_error", "could not issue a device code")
		return
	}
	display := domain.DisplayUserCode(userCode)
	w.Header().Set("Cache-Control", "no-store")
	oauthJSON(w, map[string]any{
		"device_code":               deviceCode,
		"user_code":                 display,
		"verification_uri":          s.baseURL + deviceVerifyPath,
		"verification_uri_complete": s.baseURL + deviceVerifyPath + "?user_code=" + display,
		"expires_in":                int(s.auth.OAuthDeviceCodeTTL.Seconds()),
		"interval":                  int(s.auth.OAuthDevicePollInterval.Seconds()),
	})
}

// tokenDeviceCode is the polling half: the device presents its device_code
// until the person has decided.
func (s *oauthServer) tokenDeviceCode(w http.ResponseWriter, r *http.Request) {
	clientID, ok := s.authClient(r)
	if !ok {
		oauthTokenErr(w, "invalid_client", "client authentication failed")
		return
	}
	codePlain := r.PostForm.Get("device_code")
	if !strings.HasPrefix(codePlain, oauthDeviceCodePrefix) {
		oauthTokenErr(w, "invalid_grant", "invalid device_code")
		return
	}
	now := time.Now().UTC()
	hash := hashToken(codePlain)
	d, err := s.oauth.PollDeviceCode(r.Context(), hash, now)
	if err != nil || d.ClientID != clientID {
		oauthTokenErr(w, "invalid_grant", "device_code unknown, consumed or issued to another client")
		return
	}
	if !now.Before(d.ExpiresAt) {
		oauthTokenErr(w, "expired_token", "the device code expired; request a new one")
		return
	}
	if d.LastPolledAt != nil && now.Sub(*d.LastPolledAt) < s.auth.OAuthDevicePollInterval {
		oauthTokenErr(w, "slow_down", "poll no more often than the interval")
		return
	}
	switch d.Status {
	case domain.DeviceCodePending:
		oauthTokenErr(w, "authorization_pending", "the user has not decided yet")
	case domain.DeviceCodeDenied:
		oauthTokenErr(w, "access_denied", "the user denied the request")
	case domain.DeviceCodeApproved:
		consumed, err := s.oauth.ConsumeDeviceCode(r.Context(), hash, now)
		if err != nil || consumed.UserID == nil {
			oauthTokenErr(w, "invalid_grant", "device_code already used")
			return
		}
		s.issueTokens(w, r, clientID, *consumed.UserID, consumed.GrantedScope)
	default:
		oauthTokenErr(w, "invalid_grant", "unknown device code state")
	}
}

// deviceInfo is what the verification page shows before the person decides.
func (s *oauthServer) deviceInfo(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUserNoOAuth(r, s.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	_ = userID
	pending, err := s.devices.Lookup(r.Context(), r.URL.Query().Get("user_code"), nil)
	if err != nil {
		deviceActionErr(w, err)
		return
	}
	oauthJSON(w, map[string]any{
		"client_name":      pending.Client.Name,
		"client_id":        pending.Client.ClientID,
		"software_id":      pending.Client.SoftwareID,
		"software_version": pending.Client.SoftwareVersion,
		"scopes":           scopeInfos(pending.Scopes),
		"expires_at":       pending.Code.ExpiresAt,
	})
}

// deviceDecision records approve/deny from the verification page.
func (s *oauthServer) deviceDecision(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUserNoOAuth(r, s.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var req struct {
		UserCode string `json:"user_code"`
		Approve  bool   `json:"approve"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	granted, err := s.devices.Decide(r.Context(), userID, req.UserCode, req.Approve, nil)
	if err != nil {
		deviceActionErr(w, err)
		return
	}
	oauthJSON(w, map[string]any{"approved": req.Approve, "granted_scopes": granted})
}

// resolveSessionUserNoOAuth is resolveSessionUser without OAuth access tokens:
// approving a device from the browser pages needs a session or a PAT, an
// OAuth client uses the scoped Connect RPCs.
func resolveSessionUserNoOAuth(r *http.Request, app *App) (domain.UserID, bool) {
	if tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); strings.HasPrefix(tok, oauthAccessTokenPrefix) {
		return domain.UserID{}, false
	}
	return resolveSessionUser(r, app)
}

type scopeInfo struct {
	Scope       string `json:"scope"`
	Description string `json:"description"`
	Write       bool   `json:"write"`
}

func scopeInfos(scopes []string) []scopeInfo {
	infos := make([]scopeInfo, 0, len(scopes))
	for _, sc := range scopes {
		infos = append(infos, scopeInfo{Scope: sc, Description: scopeDescription(sc), Write: domain.IsWriteScope(sc)})
	}
	return infos
}

func deviceActionErr(w http.ResponseWriter, err error) {
	var ae *port.ActionError
	status := http.StatusInternalServerError
	if errors.As(err, &ae) {
		switch ae.Kind {
		case port.ActionErrorInvalid:
			status = http.StatusBadRequest
		case port.ActionErrorNotFound:
			status = http.StatusNotFound
		}
	}
	http.Error(w, err.Error(), status)
}
