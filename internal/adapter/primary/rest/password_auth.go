package rest

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
)

// PasswordAuthHandlers serve password sign-in and the email-driven recovery
// flows. The /login page posts the sign-in form; failures bounce back to
// /login with a stable ?error= code that never says which half was wrong.
//
//	POST /auth/password            form: identifier, password, [redirect]
//	POST /auth/password/forgot     { email }        → always 200 (anti-enumeration)
//	POST /auth/password/reset      { code, password } → sets password + logs in
//	POST /auth/password/required   { challenge_token, password } → replaces a temporary password, then logs in or hands over to the second factor
//	GET  /auth/email/verify?code=…                   → verifies + redirects to /login
//	POST /auth/email/verify/send                     → resend (logged-in, unverified)
type PasswordAuthHandlers struct {
	Auth           *Auth
	Login          *authuc.PasswordLogin
	Sessions       *authuc.Sessions
	SecondFactor   *authuc.SecondFactor
	PasswordChange *authuc.PasswordChangeRequired
	Passwords      *account.Passwords
	Verification   *account.EmailVerification
	// MailEnabled: the instance has an email channel.
	MailEnabled bool
	// BaseURL is the public origin, without a trailing slash.
	BaseURL string
	// SecureCookies forces Secure cookies (https public URL).
	SecureCookies bool
	Log           *slog.Logger
}

// Mount registers the routes.
func (h *PasswordAuthHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/password", h.signIn)
	mux.HandleFunc("POST /auth/password/forgot", h.forgot)
	mux.HandleFunc("POST /auth/password/reset", h.reset)
	mux.HandleFunc("POST /auth/password/required", h.required)
	mux.HandleFunc("GET /auth/email/verify", h.verify)
	mux.HandleFunc("POST /auth/email/verify/send", h.resend)
}

func (h *PasswordAuthHandlers) signIn(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	identifier := strings.TrimSpace(strings.ToLower(r.PostFormValue("identifier")))
	password := r.PostFormValue("password")
	dest := acctSafeRedirect(r.PostFormValue("redirect"))
	if identifier == "" || password == "" {
		acctRedirectLoginError(w, r, "missing_credentials")
		return
	}
	user, err := h.Login.Login(r.Context(), identifier, password, hostOnly(clientIP(r)))
	if err != nil {
		acctRedirectLoginError(w, r, h.loginErrorCode(err))
		return
	}
	if h.redirectPasswordChange(w, r, user, dest) || h.redirectSecondFactor(w, r, user, dest) {
		return
	}
	if err := acctIssueSession(r.Context(), w, r, h.Sessions, h.SecureCookies, user.ID, domain.SessionAuthPassword); err != nil {
		h.Log.Error("password login: create session failed", "error", err, "user_id", user.ID)
		acctRedirectLoginError(w, r, "server_error")
		return
	}
	h.Log.Info("password login succeeded", "user_id", user.ID, "username", user.Username)
	http.Redirect(w, r, dest, http.StatusSeeOther) //nolint:gosec // G710: dest is constrained to a local path by acctSafeRedirect
}

// loginErrorCode maps a sign-in refusal to the /login ?error= code.
func (h *PasswordAuthHandlers) loginErrorCode(err error) string {
	switch {
	case errors.Is(err, authuc.ErrTooManyAttempts):
		return "too_many_attempts"
	case errors.Is(err, authuc.ErrInvalidCredentials):
		return "invalid_credentials"
	case errors.Is(err, authuc.ErrAccountInactive):
		return "account_inactive"
	default:
		h.Log.Error("password login failed", "error", err)
		return "server_error"
	}
}

// redirectSecondFactor hands the browser a challenge instead of a session when
// the user requires a passkey after the password; reports whether it did.
func (h *PasswordAuthHandlers) redirectSecondFactor(w http.ResponseWriter, r *http.Request, user domain.User, dest string) bool {
	if !h.SecondFactor.Required(user) {
		return false
	}
	token, err := h.SecondFactor.Begin(r.Context(), user.ID)
	if err != nil {
		h.Log.Error("password login: begin second factor failed", "error", err, "user_id", user.ID)
		acctRedirectLoginError(w, r, "server_error")
		return true
	}
	acctRedirectChallenge(w, r, "challenge", token, dest)
	return true
}

// redirectPasswordChange hands the browser a password-change challenge when
// the user must replace a temporary password; POST /auth/password/required
// then completes the sign-in.
func (h *PasswordAuthHandlers) redirectPasswordChange(w http.ResponseWriter, r *http.Request, user domain.User, dest string) bool {
	if !h.PasswordChange.Required(user) {
		return false
	}
	token, err := h.PasswordChange.Begin(r.Context(), user.ID)
	if err != nil {
		h.Log.Error("password login: begin password change failed", "error", err, "user_id", user.ID)
		acctRedirectLoginError(w, r, "server_error")
		return true
	}
	acctRedirectChallenge(w, r, "set_password", token, dest)
	return true
}

func (h *PasswordAuthHandlers) forgot(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !h.MailEnabled {
		h.Log.Warn("password reset requested but email is disabled")
	}
	// Always respond the same way regardless of whether the email exists.
	if _, err := h.Passwords.RequestReset(r.Context(), body.Email); err != nil {
		h.Log.Warn("password reset mail", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "If that email belongs to an account, a reset link is on its way.",
	})
}

func (h *PasswordAuthHandlers) reset(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	userID, err := h.Passwords.ConfirmReset(r.Context(), strings.TrimSpace(body.Code), body.Password)
	if err != nil {
		writeActionError(w, err, "reset failed")
		return
	}
	// The password is set either way; without a session they sign in manually.
	loggedIn := acctIssueSession(r.Context(), w, r, h.Sessions, h.SecureCookies, userID, domain.SessionAuthPassword) == nil
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logged_in": loggedIn})
}

// required completes a must-change-password sign-in with the new password;
// a user who also needs a passkey gets the second-factor challenge instead.
func (h *PasswordAuthHandlers) required(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	if h.PasswordChange == nil {
		http.Error(w, "password change not available", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Token    string `json:"challenge_token"`
		Password string `json:"password"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	user, err := h.PasswordChange.Complete(r.Context(), strings.TrimSpace(body.Token), body.Password)
	if err != nil {
		writeActionError(w, err, "password change failed")
		return
	}
	if h.SecondFactor.Required(user) {
		token, err := h.SecondFactor.Begin(r.Context(), user.ID)
		if err != nil {
			h.Log.Error("password change: begin second factor failed", "error", err, "user_id", user.ID)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logged_in": false, "challenge": token})
		return
	}
	loggedIn := acctIssueSession(r.Context(), w, r, h.Sessions, h.SecureCookies, user.ID, domain.SessionAuthPassword) == nil
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logged_in": loggedIn})
}

func (h *PasswordAuthHandlers) verify(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" || h.Verification.Confirm(r.Context(), code) != nil {
		http.Redirect(w, r, h.BaseURL+"/login?verify_error=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, h.BaseURL+"/login?verified=1", http.StatusSeeOther)
}

func (h *PasswordAuthHandlers) resend(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	already, err := h.Verification.Resend(r.Context(), userID)
	if err != nil {
		writeActionError(w, err, "send failed")
		return
	}
	if already {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_verified": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sent": true})
}
