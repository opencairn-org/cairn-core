package main

import (
	"crypto/sha256"
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
)

// WebAuthn / passkey endpoints (REST — go-webauthn already emits the standard
// WebAuthn JSON the browser's navigator.credentials consumes, so a typed proto
// wrapper would be pure overhead).
//
//	POST /auth/webauthn/register/start    (authed) → CredentialCreation options
//	POST /auth/webauthn/register/finish   (authed) ← attestation; stores passkey
//	POST /auth/webauthn/login/start                → CredentialAssertion options
//	POST /auth/webauthn/login/finish               ← assertion; issues a session
//	GET    /api/passkeys                  (authed) → list
//	PUT    /api/passkeys/{id}             (authed) ← {name}; rename
//	DELETE /api/passkeys/{id}             (authed)
const (
	waRegCookie   = "cairn_wa_reg"
	waLoginCookie = "cairn_wa_login"
)

func mountWebAuthn(mux *http.ServeMux, app *App, logger *slog.Logger, publicURL string, sessionTTL time.Duration) {
	if app.Passkeys == nil || app.Users == nil || app.Sessions == nil {
		return
	}
	wa, err := newWebAuthn(publicURL)
	if err != nil {
		logger.Error("webauthn disabled: bad config", "error", err)
		return
	}
	h := &waHandlers{app: app, logger: logger, wa: wa, sessionTTL: sessionTTL}

	// ---- registration (authenticated) ------------------------------------
	mux.HandleFunc("POST /auth/webauthn/register/start", h.registerStart)
	mux.HandleFunc("POST /auth/webauthn/register/finish", h.registerFinish)

	// ---- passwordless login ----------------------------------------------
	mux.HandleFunc("POST /auth/webauthn/login/start", h.loginStart)
	mux.HandleFunc("POST /auth/webauthn/login/finish", h.loginFinish)

	// ---- passkey management (authenticated) ------------------------------
	mux.HandleFunc("POST /auth/2fa/complete", h.completeSecondFactor)
	mux.HandleFunc("PUT /api/passkeys/second-factor", h.setSecondFactor)
	mux.HandleFunc("GET /api/passkeys", h.list)
	mux.HandleFunc("PUT /api/passkeys/{id}", h.rename)
	mux.HandleFunc("DELETE /api/passkeys/{id}", h.delete)

	logger.Info("webauthn endpoints mounted", "rpid", wa.Config.RPID, "origins", wa.Config.RPOrigins)
}

// waHandlers carries the shared state of the WebAuthn routes.
type waHandlers struct {
	app        *App
	logger     *slog.Logger
	wa         *webauthn.WebAuthn
	sessionTTL time.Duration
}

func (h *waHandlers) registerStart(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	waUser, err := loadWAUser(r, h.app, userID)
	if err != nil {
		http.Error(w, "user load failed", http.StatusInternalServerError)
		return
	}
	// Exclude already-registered credentials; require a discoverable
	// (resident) key so this passkey also works for usernameless login.
	exclusions := make([]protocol.CredentialDescriptor, 0, len(waUser.creds))
	for _, c := range waUser.creds {
		exclusions = append(exclusions, c.Descriptor())
	}
	options, session, err := h.wa.BeginRegistration(waUser,
		webauthn.WithExclusions(exclusions),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
	)
	if err != nil {
		http.Error(w, "begin registration failed", http.StatusInternalServerError)
		return
	}
	storeWASession(w, r, waRegCookie, session)
	writeJSON(w, http.StatusOK, options)
}

func (h *waHandlers) registerFinish(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	session, err := loadWASession(r, waRegCookie)
	if err != nil {
		http.Error(w, "no registration in progress", http.StatusBadRequest)
		return
	}
	waUser, err := loadWAUser(r, h.app, userID)
	if err != nil {
		http.Error(w, "user load failed", http.StatusInternalServerError)
		return
	}
	cred, err := h.wa.FinishRegistration(waUser, *session, r)
	if err != nil {
		h.logger.Warn("webauthn registration failed", "error", err)
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
	if _, err := h.app.Passkeys.Create(r.Context(), domain.Passkey{
		UserID: userID, CredentialID: cred.ID, CredentialJSON: credJSON, Name: name,
	}); err != nil {
		http.Error(w, "store passkey failed", http.StatusInternalServerError)
		return
	}
	clearWACookie(w, r, waRegCookie)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *waHandlers) loginStart(w http.ResponseWriter, r *http.Request) {
	options, session, err := h.wa.BeginDiscoverableLogin()
	if err != nil {
		http.Error(w, "begin login failed", http.StatusInternalServerError)
		return
	}
	storeWASession(w, r, waLoginCookie, session)
	writeJSON(w, http.StatusOK, options)
}

func (h *waHandlers) loginFinish(w http.ResponseWriter, r *http.Request) {
	session, err := loadWASession(r, waLoginCookie)
	if err != nil {
		http.Error(w, "no login in progress", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	// DiscoverableUserHandler resolves the user from the credential's user
	// handle (which we set to the user's UUID at registration).
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		uid, err := uuid.FromBytes(userHandle)
		if err != nil {
			return nil, err
		}
		waUser, err := loadWAUser(r, h.app, domain.UserID(uid))
		return waUser, err
	}
	cred, err := h.wa.FinishDiscoverableLogin(handler, *session, r)
	if err != nil {
		h.logger.Warn("webauthn login failed", "error", err)
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}
	// Persist the advanced sign counter, and resolve the owning user.
	credJSON, _ := json.Marshal(cred)
	_ = h.app.Passkeys.UpdateCredential(ctx, cred.ID, credJSON)
	pk, err := h.app.Passkeys.GetByCredentialID(ctx, cred.ID)
	if err != nil {
		http.Error(w, "credential not found", http.StatusUnauthorized)
		return
	}
	clearWACookie(w, r, waLoginCookie)
	// With ?challenge= the assertion is the second factor of a password
	// sign-in: it satisfies the challenge, the session comes from /auth/2fa/complete.
	if challenge := r.URL.Query().Get("challenge"); challenge != "" {
		if h.app.SecondFactor == nil {
			http.Error(w, "second factor not available", http.StatusServiceUnavailable)
			return
		}
		if err := h.app.SecondFactor.Satisfy(ctx, challenge, pk.UserID); err != nil {
			writeActionError(w, err, "challenge failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "challenge_satisfied": true})
		return
	}
	if err := issueSessionForUser(w, r, h.app, pk.UserID, domain.SessionAuthPasskey, h.sessionTTL); err != nil {
		h.logger.Error("webauthn login: issue session failed", "error", err)
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *waHandlers) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	pks, err := h.app.Passkeys.ListByUser(r.Context(), userID)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(pks))
	for _, p := range pks {
		var lastUsed any
		if p.LastUsedAt != nil {
			lastUsed = p.LastUsedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, map[string]any{
			"id": p.ID.String(), "name": p.Name,
			"created_at": p.CreatedAt.UTC().Format(time.RFC3339), "last_used_at": lastUsed,
		})
	}
	user, _ := h.app.Users.GetUser(r.Context(), userID)
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": out, "second_factor": user.PasskeySecondFactor})
}

// completeSecondFactor turns a satisfied challenge into a cookie session.
//
//	POST /auth/2fa/complete { challenge_token }
func (h *waHandlers) completeSecondFactor(w http.ResponseWriter, r *http.Request) {
	if h.app.SecondFactor == nil {
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
	userID, err := h.app.SecondFactor.Complete(r.Context(), body.Token)
	if err != nil {
		writeActionError(w, err, "challenge failed")
		return
	}
	if err := issueSessionForUser(w, r, h.app, userID, domain.SessionAuthPasswordWithWebauthn, h.sessionTTL); err != nil {
		h.logger.Error("second factor: issue session failed", "error", err)
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// setSecondFactor opts the caller in or out of the passkey second factor.
//
//	PUT /api/passkeys/second-factor { enabled }
func (h *waHandlers) setSecondFactor(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if h.app.SecondFactor == nil {
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
	if err := h.app.SecondFactor.SetEnabled(r.Context(), userID, body.Enabled); err != nil {
		writeActionError(w, err, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "second_factor": body.Enabled})
}

func (h *waHandlers) rename(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := h.app.Passkeys.Rename(r.Context(), userID, domain.PasskeyID(id), strings.TrimSpace(body.Name)); err != nil {
		http.Error(w, "rename failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *waHandlers) delete(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.app.Passkeys.Delete(r.Context(), userID, domain.PasskeyID(id)); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// webAuthnUser adapts a Cairn user + its passkeys to the webauthn.User interface.
type webAuthnUser struct {
	id      domain.UserID
	name    string
	display string
	creds   []webauthn.Credential
}

func (u *webAuthnUser) WebAuthnID() []byte {
	b := u.id.UUID()
	return b[:]
}
func (u *webAuthnUser) WebAuthnName() string                       { return u.name }
func (u *webAuthnUser) WebAuthnDisplayName() string                { return u.display }
func (u *webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }
func (u *webAuthnUser) WebAuthnIcon() string                       { return "" }

// loadWAUser hydrates a webAuthnUser (user + decoded passkey credentials).
func loadWAUser(r *http.Request, app *App, userID domain.UserID) (*webAuthnUser, error) {
	u, err := app.Users.GetUser(r.Context(), userID)
	if err != nil {
		return nil, err
	}
	pks, err := app.Passkeys.ListByUser(r.Context(), userID)
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
	return &webAuthnUser{id: u.ID, name: name, display: display, creds: creds}, nil
}

// issueSessionForUser creates a session with the given auth method and sets the
// cookie. Mirrors the password-login path; shared by passkey login and invite
// signup.
func issueSessionForUser(w http.ResponseWriter, r *http.Request, app *App, userID domain.UserID, method domain.SessionAuthMethod, ttl time.Duration) error {
	token, err := randomSessionToken()
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(token))
	now := time.Now()
	expiresAt := now.Add(ttl)
	if _, err := app.Sessions.Create(r.Context(), domain.Session{
		UserID:           userID,
		TokenHash:        hash[:],
		AuthMethod:       method,
		UserAgentSummary: truncateUA(r.UserAgent()),
		CreatedAt:        now,
		LastSeenAt:       now,
		ExpiresAt:        expiresAt,
	}); err != nil {
		return err
	}
	setSessionCookie(w, r, token, expiresAt)
	return nil
}

func newWebAuthn(publicURL string) (*webauthn.WebAuthn, error) {
	u, err := url.Parse(publicURL)
	if err != nil {
		return nil, err
	}
	origin := strings.TrimRight(publicURL, "/")
	return webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(), // e.g. "localhost" or "cairn.example.com"
		RPDisplayName: "Cairn",
		RPOrigins:     []string{origin},
	})
}

func storeWASession(w http.ResponseWriter, r *http.Request, name string, sd *webauthn.SessionData) {
	b, _ := json.Marshal(sd)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name: name, Value: base64.RawURLEncoding.EncodeToString(b),
		Path: "/auth/webauthn/", HttpOnly: true, Secure: cookieSecure(r),
		SameSite: http.SameSiteLaxMode, MaxAge: 300,
	})
}

func loadWASession(r *http.Request, name string) (*webauthn.SessionData, error) {
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

func clearWACookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name: name, Value: "", Path: "/auth/webauthn/", HttpOnly: true,
		Secure: cookieSecure(r), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}
