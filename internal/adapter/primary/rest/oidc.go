package rest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
)

// OIDCHandlers serve OIDC sign-in and the mechanism-agnostic logout.
// Providers come from the environment; with OIDC nil only logout mounts.
//
//	POST /auth/logout                     → revoke session, clear cookie; browsers go to /login
//	GET  /auth/oidc/clients               → configured providers (login page)
//	GET  /auth/oidc/{provider}/start      → redirect to IdP authorize
//	GET  /auth/oidc/callback              → exchange code, set session
type OIDCHandlers struct {
	OIDC     *authuc.OIDCLogin
	Sessions *authuc.Sessions
	// FlowBox seals the login-flow cookie; nil leaves it plain.
	FlowBox interface {
		Encrypt(plaintext, associatedData []byte) ([]byte, error)
		Decrypt(ciphertext, associatedData []byte) ([]byte, error)
	}
	SecureCookies bool
	Log           *slog.Logger
}

// Mount registers the routes.
func (h *OIDCHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/logout", h.logout)
	if h.OIDC == nil {
		return
	}
	mux.HandleFunc("GET /auth/oidc/clients", h.clients)
	mux.HandleFunc("GET /auth/oidc/{provider}/start", h.start)
	mux.HandleFunc("GET /auth/oidc/callback", h.callback)
}

func (h *OIDCHandlers) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" && h.Sessions != nil {
		h.Sessions.End(r.Context(), c.Value)
	}
	acctClearSessionCookie(w, r, h.SecureCookies)
	// A form POST navigates; without a redirect the browser stays on the signed-in page.
	if r.Header.Get("Sec-Fetch-Mode") == "navigate" || strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *OIDCHandlers) clients(w http.ResponseWriter, _ *http.Request) {
	type clientJSON struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	providers := h.OIDC.Providers()
	out := make([]clientJSON, len(providers))
	for i, p := range providers {
		out[i] = clientJSON{ID: p.ID, DisplayName: p.Name}
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": out})
}

func (h *OIDCHandlers) start(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("provider")
	out, err := h.OIDC.StartLogin(r.Context(), providerID)
	if err != nil {
		h.Log.Warn("oidc start failed", "provider", providerID, "error", err)
		status := http.StatusBadGateway
		if errors.Is(err, authuc.ErrProviderNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, "oidc start: "+err.Error(), status)
		return
	}
	flow := acctLoginFlow{
		Provider:     providerID,
		State:        out.State,
		Nonce:        out.Nonce,
		CodeVerifier: out.CodeVerifier,
		CreatedAt:    time.Now().Unix(),
	}
	if err := h.setFlowCookie(w, r, flow); err != nil {
		http.Error(w, "cookie write: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, out.AuthURL, http.StatusFound)
}

func (h *OIDCHandlers) callback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		acctRedirectLoginError(w, r, "oidc_failed")
		return
	}
	flow, err := h.readFlowCookie(r)
	if err != nil {
		// Expired, missing or tampered: took too long or came back in another browser.
		acctRedirectLoginError(w, r, "oidc_expired")
		return
	}
	h.clearFlowCookie(w, r)
	if state != flow.State {
		acctRedirectLoginError(w, r, "oidc_state")
		return
	}
	if flow.Provider == "" {
		acctRedirectLoginError(w, r, "oidc_failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := h.OIDC.CompleteLogin(ctx, authuc.CompleteLoginInput{
		ProviderID:    flow.Provider,
		Code:          code,
		CodeVerifier:  flow.CodeVerifier,
		ExpectedNonce: flow.Nonce,
		ClientIP:      clientIP(r),
		UserAgent:     r.UserAgent(),
	})
	if err != nil {
		h.Log.Warn("oidc callback failed", "error", err)
		acctRedirectLoginError(w, r, acctOIDCErrorCode(err))
		return
	}
	acctSetSessionCookie(w, r, h.SecureCookies, res.BearerToken, res.ExpiresAt)
	http.Redirect(w, r, "/", http.StatusFound)
}

// acctLoginFlow rides in the cairn_login_flow cookie from /start to /callback.
type acctLoginFlow struct {
	Provider     string `json:"provider"`
	State        string `json:"state"`
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"code_verifier,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

const (
	acctLoginFlowCookie = "cairn_login_flow"
	// acctLoginFlowAAD binds the sealed flow cookie to its purpose.
	acctLoginFlowAAD = "cairn_login_flow"
	acctLoginFlowTTL = 10 * time.Minute
)

func (h *OIDCHandlers) setFlowCookie(w http.ResponseWriter, r *http.Request, s acctLoginFlow) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	// Sealed so a planted cookie can't force a chosen nonce or PKCE verifier.
	if h.FlowBox != nil {
		if body, err = h.FlowBox.Encrypt(body, []byte(acctLoginFlowAAD)); err != nil {
			return err
		}
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name:     acctLoginFlowCookie,
		Value:    base64.RawURLEncoding.EncodeToString(body),
		Path:     "/auth/oidc/",
		HttpOnly: true,
		Secure:   acctCookieSecure(r, h.SecureCookies),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(acctLoginFlowTTL.Seconds()),
	})
	return nil
}

func (h *OIDCHandlers) readFlowCookie(r *http.Request) (acctLoginFlow, error) {
	c, err := r.Cookie(acctLoginFlowCookie)
	if err != nil {
		return acctLoginFlow{}, err
	}
	body, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return acctLoginFlow{}, err
	}
	if h.FlowBox != nil {
		if body, err = h.FlowBox.Decrypt(body, []byte(acctLoginFlowAAD)); err != nil {
			return acctLoginFlow{}, fmt.Errorf("login flow cookie integrity check failed: %w", err)
		}
	}
	var s acctLoginFlow
	if err := json.Unmarshal(body, &s); err != nil {
		return acctLoginFlow{}, err
	}
	if time.Since(time.Unix(s.CreatedAt, 0)) > acctLoginFlowTTL {
		return acctLoginFlow{}, errors.New("login flow cookie expired")
	}
	return s, nil
}

func (h *OIDCHandlers) clearFlowCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name:     acctLoginFlowCookie,
		Value:    "",
		Path:     "/auth/oidc/",
		HttpOnly: true,
		Secure:   acctCookieSecure(r, h.SecureCookies),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// acctOIDCErrorCode maps a CompleteLogin failure to a stable /login ?error= code.
func acctOIDCErrorCode(err error) string {
	switch {
	case errors.Is(err, authuc.ErrProviderNotFound):
		return "oidc_unavailable"
	case errors.Is(err, authuc.ErrAutoProvisionDenied):
		return "auto_provision_denied"
	case errors.Is(err, authuc.ErrSubjectMissing), errors.Is(err, authuc.ErrEmailMissing):
		return "oidc_bad_response"
	case errors.Is(err, authuc.ErrIDTokenVerifyFailed):
		return "oidc_verify_failed"
	default:
		return "oidc_failed"
	}
}
