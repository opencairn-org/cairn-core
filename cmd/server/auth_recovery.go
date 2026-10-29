package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

const emailVerificationTTL = 24 * time.Hour

// mountAuthRecovery wires the email-driven account recovery flows: password
// reset and email verification. Both lean on the email channel (app.Email);
// when email is disabled the request endpoints still return a generic 200 so
// they never reveal whether an address is registered.
//
//	POST /auth/password/forgot     { email }        → always 200 (anti-enumeration)
//	POST /auth/password/reset      { code, password } → sets password + logs in
//	POST /auth/password/required   { challenge_token, password } → replaces a temporary password, then logs in or hands over to the second factor
//	GET  /auth/email/verify?code=…                   → verifies + redirects to /login
//	POST /auth/email/verify/send                     → resend (logged-in, unverified)
func mountAuthRecovery(mux *http.ServeMux, app *App, logger *slog.Logger, publicURL string, sessionTTL time.Duration) {
	h := &authRecoveryHandlers{app: app, logger: logger, base: strings.TrimRight(publicURL, "/"), sessionTTL: sessionTTL}

	// ---- password: forgot ----
	mux.HandleFunc("POST /auth/password/forgot", h.forgot)

	// ---- password: reset ----
	mux.HandleFunc("POST /auth/password/reset", h.reset)

	// ---- password: forced change at sign-in ----
	mux.HandleFunc("POST /auth/password/required", h.required)

	// ---- email: verify (GET, from the email link) ----
	mux.HandleFunc("GET /auth/email/verify", h.verify)

	// ---- email: resend verification (logged-in, unverified) ----
	mux.HandleFunc("POST /auth/email/verify/send", h.resend)

	logger.Info("auth recovery endpoints mounted (password reset + email verification)")
}

// authRecoveryHandlers carries the shared state of the recovery routes.
type authRecoveryHandlers struct {
	app        *App
	logger     *slog.Logger
	base       string
	sessionTTL time.Duration
}

// authRecoveryGeneric is the identical reply whether or not the email exists.
func authRecoveryGeneric(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "If that email belongs to an account, a reset link is on its way.",
	})
}

func (h *authRecoveryHandlers) forgot(w http.ResponseWriter, r *http.Request) {
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
	if h.app.Email == nil {
		h.logger.Warn("password reset requested but email is disabled")
	}
	// Always respond the same way regardless of whether the email exists.
	if _, err := h.app.Passwords.RequestReset(r.Context(), body.Email); err != nil {
		h.logger.Warn("password reset mail", "error", err)
	}
	authRecoveryGeneric(w)
}

func (h *authRecoveryHandlers) reset(w http.ResponseWriter, r *http.Request) {
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
	userID, err := h.app.Passwords.ConfirmReset(r.Context(), strings.TrimSpace(body.Code), body.Password)
	if err != nil {
		writeActionError(w, err, "reset failed")
		return
	}
	// Auto-login after a successful reset.
	if err := issueSessionForUser(w, r, h.app, userID, domain.SessionAuthPassword, h.sessionTTL); err != nil {
		// Password is set; just have them log in manually.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logged_in": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logged_in": true})
}

// required completes a must-change-password sign-in with the new password.
// A user who also needs a passkey gets the second-factor challenge back
// instead of a session.
func (h *authRecoveryHandlers) required(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	if h.app.PasswordChange == nil {
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
	user, err := h.app.PasswordChange.Complete(r.Context(), strings.TrimSpace(body.Token), body.Password)
	if err != nil {
		writeActionError(w, err, "password change failed")
		return
	}
	if h.app.SecondFactor.Required(user) {
		token, err := h.app.SecondFactor.Begin(r.Context(), user.ID)
		if err != nil {
			h.logger.Error("password change: begin second factor failed", "error", err, "user_id", user.ID)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logged_in": false, "challenge": token})
		return
	}
	if err := issueSessionForUser(w, r, h.app, user.ID, domain.SessionAuthPassword, h.sessionTTL); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logged_in": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logged_in": true})
}

func (h *authRecoveryHandlers) verify(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		http.Redirect(w, r, h.base+"/login?verify_error=1", http.StatusSeeOther)
		return
	}
	hash := sha256.Sum256([]byte(code))
	userID, _, err := h.app.AuthTokens.ConsumeEmailVerificationToken(r.Context(), hash[:], time.Now().UTC())
	if err != nil {
		http.Redirect(w, r, h.base+"/login?verify_error=1", http.StatusSeeOther)
		return
	}
	if err := h.app.Users.UpdateEmailVerified(r.Context(), userID, true); err != nil {
		http.Redirect(w, r, h.base+"/login?verify_error=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, h.base+"/login?verified=1", http.StatusSeeOther)
}

func (h *authRecoveryHandlers) resend(w http.ResponseWriter, r *http.Request) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	user, err := h.app.Users.GetUser(r.Context(), userID)
	if err != nil {
		http.Error(w, "user lookup failed", http.StatusInternalServerError)
		return
	}
	if user.EmailVerified {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_verified": true})
		return
	}
	if h.app.Email == nil || user.Email == "" {
		http.Error(w, "email is not available on this instance/account", http.StatusServiceUnavailable)
		return
	}
	if err := sendVerificationEmail(r.Context(), h.app, h.logger, h.base, user); err != nil {
		http.Error(w, "send failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sent": true})
}

// sendVerificationEmail issues an email-verification token and mails the link.
// Reusable from the signup path. Best-effort: returns the send error so the
// caller can decide (signup ignores it; the resend endpoint surfaces it).
func sendVerificationEmail(ctx context.Context, app *App, logger *slog.Logger, base string, user domain.User) error {
	if app.Email == nil || user.Email == "" {
		return errors.New("email unavailable")
	}
	code, hash := newRecoveryCode()
	if err := app.AuthTokens.CreateEmailVerificationToken(ctx, user.ID, user.Email, hash, time.Now().UTC().Add(emailVerificationTTL)); err != nil {
		logger.Error("create email verification token", "error", err)
		return err
	}
	link := base + "/auth/email/verify?code=" + code
	return app.Email.Send(ctx, port.EmailMessage{
		To:      user.Email,
		Subject: "Verify your Cairn email",
		TextBody: "Welcome to Cairn! Confirm this email address by opening the link below " +
			"(valid for 24 hours):\n" + link + "\n\n- Cairn",
		HTMLBody: "<p>Welcome to Cairn! Confirm this email address:</p>" +
			"<p><a href=\"" + link + "\">Verify email</a> (valid for 24 hours).</p><p>- Cairn</p>",
	})
}

// newRecoveryCode returns a URL-safe random code plus its sha256 (what we store).
func newRecoveryCode() (code string, hash []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	code = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(code))
	return code, h[:]
}
