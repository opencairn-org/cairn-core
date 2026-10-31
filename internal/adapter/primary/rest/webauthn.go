package rest

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
)

// WebAuthnHandlers serve the passkey ceremonies and passkey management.
// go-webauthn already emits the WebAuthn JSON navigator.credentials consumes.
//
//	POST   /auth/webauthn/register/start   (authed) → CredentialCreation options
//	POST   /auth/webauthn/register/finish  (authed) ← attestation; stores passkey
//	POST   /auth/webauthn/login/start               → CredentialAssertion options
//	POST   /auth/webauthn/login/finish              ← assertion; issues a session (or satisfies ?challenge=)
//	POST   /auth/2fa/complete                       { challenge_token } → session
//	PUT    /api/passkeys/second-factor     (authed) { enabled }
//	GET    /api/passkeys                   (authed) → list
//	PUT    /api/passkeys/{id}              (authed) ← {name}; rename
//	DELETE /api/passkeys/{id}              (authed)
type WebAuthnHandlers struct {
	Auth          *Auth
	WebAuthn      *webauthn.WebAuthn
	Passkeys      *account.Passkeys
	Sessions      *authuc.Sessions
	SecondFactor  *authuc.SecondFactor
	SecureCookies bool
	Log           *slog.Logger
}

// NewWebAuthn is the relying party for the public origin.
func NewWebAuthn(publicURL string) (*webauthn.WebAuthn, error) {
	u, err := url.Parse(publicURL)
	if err != nil {
		return nil, err
	}
	return webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(), // e.g. "localhost" or "cairn.example.com"
		RPDisplayName: "Cairn",
		RPOrigins:     []string{strings.TrimRight(publicURL, "/")},
	})
}

// Mount registers the routes.
func (h *WebAuthnHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/webauthn/register/start", h.registerStart)
	mux.HandleFunc("POST /auth/webauthn/register/finish", h.registerFinish)
	mux.HandleFunc("POST /auth/webauthn/login/start", h.loginStart)
	mux.HandleFunc("POST /auth/webauthn/login/finish", h.loginFinish)
	mux.HandleFunc("POST /auth/2fa/complete", h.completeSecondFactor)
	mux.HandleFunc("PUT /api/passkeys/second-factor", h.setSecondFactor)
	mux.HandleFunc("GET /api/passkeys", h.list)
	mux.HandleFunc("PUT /api/passkeys/{id}", h.rename)
	mux.HandleFunc("DELETE /api/passkeys/{id}", h.delete)
}

const (
	acctWARegCookie   = "cairn_wa_reg"
	acctWALoginCookie = "cairn_wa_login"
)

func (h *WebAuthnHandlers) registerStart(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	waUser, err := h.holder(r, userID)
	if err != nil {
		http.Error(w, "user load failed", http.StatusInternalServerError)
		return
	}
	// Exclude registered credentials; a resident key also serves usernameless login.
	exclusions := make([]protocol.CredentialDescriptor, 0, len(waUser.creds))
	for _, c := range waUser.creds {
		exclusions = append(exclusions, c.Descriptor())
	}
	options, session, err := h.WebAuthn.BeginRegistration(waUser,
		webauthn.WithExclusions(exclusions),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
	)
	if err != nil {
		http.Error(w, "begin registration failed", http.StatusInternalServerError)
		return
	}
	h.storeSession(w, r, acctWARegCookie, session)
	writeJSON(w, http.StatusOK, options)
}

func (h *WebAuthnHandlers) registerFinish(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	session, err := acctLoadWASession(r, acctWARegCookie)
	if err != nil {
		http.Error(w, "no registration in progress", http.StatusBadRequest)
		return
	}
	waUser, err := h.holder(r, userID)
	if err != nil {
		http.Error(w, "user load failed", http.StatusInternalServerError)
		return
	}
	cred, err := h.WebAuthn.FinishRegistration(waUser, *session, r)
	if err != nil {
		h.Log.Warn("webauthn registration failed", "error", err)
		http.Error(w, "registration verification failed", http.StatusBadRequest)
		return
	}
	credJSON, err := json.Marshal(cred)
	if err != nil {
		http.Error(w, "encode credential failed", http.StatusInternalServerError)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Passkey"
	}
	if err := h.Passkeys.Register(r.Context(), userID, cred.ID, credJSON, name); err != nil {
		http.Error(w, "store passkey failed", http.StatusInternalServerError)
		return
	}
	h.clearCookie(w, r, acctWARegCookie)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *WebAuthnHandlers) loginStart(w http.ResponseWriter, r *http.Request) {
	options, session, err := h.WebAuthn.BeginDiscoverableLogin()
	if err != nil {
		http.Error(w, "begin login failed", http.StatusInternalServerError)
		return
	}
	h.storeSession(w, r, acctWALoginCookie, session)
	writeJSON(w, http.StatusOK, options)
}

func (h *WebAuthnHandlers) loginFinish(w http.ResponseWriter, r *http.Request) {
	session, err := acctLoadWASession(r, acctWALoginCookie)
	if err != nil {
		http.Error(w, "no login in progress", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	// The user handle is the user's UUID, set at registration.
	handler := func(_, userHandle []byte) (webauthn.User, error) {
		uid, err := uuid.FromBytes(userHandle)
		if err != nil {
			return nil, err
		}
		return h.holder(r, domain.UserID(uid))
	}
	cred, err := h.WebAuthn.FinishDiscoverableLogin(handler, *session, r)
	if err != nil {
		h.Log.Warn("webauthn login failed", "error", err)
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}
	credJSON, _ := json.Marshal(cred)
	pk, err := h.Passkeys.Used(ctx, cred.ID, credJSON)
	if err != nil {
		http.Error(w, "credential not found", http.StatusUnauthorized)
		return
	}
	h.clearCookie(w, r, acctWALoginCookie)
	// With ?challenge= the assertion is the second factor of a password
	// sign-in; the session then comes from /auth/2fa/complete.
	if challenge := r.URL.Query().Get("challenge"); challenge != "" {
		if h.SecondFactor == nil {
			http.Error(w, "second factor not available", http.StatusServiceUnavailable)
			return
		}
		if err := h.SecondFactor.Satisfy(ctx, challenge, pk.UserID); err != nil {
			writeActionError(w, err, "challenge failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "challenge_satisfied": true})
		return
	}
	if err := acctIssueSession(ctx, w, r, h.Sessions, h.SecureCookies, pk.UserID, domain.SessionAuthPasskey); err != nil {
		h.Log.Error("webauthn login: issue session failed", "error", err)
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *WebAuthnHandlers) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	pks, secondFactor, err := h.Passkeys.List(r.Context(), userID)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(pks))
	for _, p := range pks {
		out = append(out, map[string]any{
			"id": p.ID.String(), "name": p.Name,
			"created_at": p.CreatedAt.UTC().Format(time.RFC3339), "last_used_at": acctFmtTimePtr(p.LastUsedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": out, "second_factor": secondFactor})
}

// completeSecondFactor turns a satisfied challenge into a cookie session.
func (h *WebAuthnHandlers) completeSecondFactor(w http.ResponseWriter, r *http.Request) {
	if h.SecondFactor == nil {
		http.Error(w, "second factor not available", http.StatusServiceUnavailable)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Token string `json:"challenge_token"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	userID, err := h.SecondFactor.Complete(r.Context(), body.Token)
	if err != nil {
		writeActionError(w, err, "challenge failed")
		return
	}
	if err := acctIssueSession(r.Context(), w, r, h.Sessions, h.SecureCookies, userID, domain.SessionAuthPasswordWithWebauthn); err != nil {
		h.Log.Error("second factor: issue session failed", "error", err)
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// setSecondFactor opts the caller in or out of the passkey second factor.
func (h *WebAuthnHandlers) setSecondFactor(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if h.SecondFactor == nil {
		http.Error(w, "second factor not available", http.StatusServiceUnavailable)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.SecondFactor.SetEnabled(r.Context(), userID, body.Enabled); err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "second_factor": body.Enabled})
}

func (h *WebAuthnHandlers) rename(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	id, err := domain.ParseUUID[domain.PasskeyID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.Passkeys.Rename(r.Context(), userID, id, strings.TrimSpace(body.Name)); err != nil {
		http.Error(w, "rename failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *WebAuthnHandlers) delete(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.PasskeyID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.Passkeys.Delete(r.Context(), userID, id); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// acctWAUser adapts a Cairn user and their passkeys to webauthn.User.
type acctWAUser struct {
	id      domain.UserID
	name    string
	display string
	creds   []webauthn.Credential
}

func (u *acctWAUser) WebAuthnID() []byte {
	b := u.id.UUID()
	return b[:]
}
func (u *acctWAUser) WebAuthnName() string                       { return u.name }
func (u *acctWAUser) WebAuthnDisplayName() string                { return u.display }
func (u *acctWAUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }
func (u *acctWAUser) WebAuthnIcon() string                       { return "" }

// holder loads the user with their decoded credentials.
func (h *WebAuthnHandlers) holder(r *http.Request, userID domain.UserID) (*acctWAUser, error) {
	u, pks, err := h.Passkeys.Holder(r.Context(), userID)
	if err != nil {
		return nil, err
	}
	creds := make([]webauthn.Credential, 0, len(pks))
	for _, p := range pks {
		var c webauthn.Credential
		if json.Unmarshal(p.CredentialJSON, &c) == nil {
			creds = append(creds, c)
		}
	}
	name := u.Username
	if name == "" {
		name = u.Email
	}
	display := u.DisplayName
	if display == "" {
		display = name
	}
	return &acctWAUser{id: u.ID, name: name, display: display, creds: creds}, nil
}

func (h *WebAuthnHandlers) storeSession(w http.ResponseWriter, r *http.Request, name string, sd *webauthn.SessionData) {
	b, _ := json.Marshal(sd)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name: name, Value: base64.RawURLEncoding.EncodeToString(b),
		Path: "/auth/webauthn/", HttpOnly: true, Secure: acctCookieSecure(r, h.SecureCookies),
		SameSite: http.SameSiteLaxMode, MaxAge: 300,
	})
}

func acctLoadWASession(r *http.Request, name string) (*webauthn.SessionData, error) {
	c, err := r.Cookie(name)
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil, err
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal(raw, &sd); err != nil {
		return nil, err
	}
	return &sd, nil
}

func (h *WebAuthnHandlers) clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name: name, Value: "", Path: "/auth/webauthn/", HttpOnly: true,
		Secure: acctCookieSecure(r, h.SecureCookies), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}
