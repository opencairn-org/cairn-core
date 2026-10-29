package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// mountPasswordAuth registers the username/email + password sign-in
// endpoint that the SvelteKit /login page posts to.
//
//	POST /auth/password    form: identifier, password, [redirect]
//
// On success it issues a server-side session (AuthMethod=password), sets
// the cairn_session cookie — the same cookie the OIDC callback sets and
// the Connect-RPC SessionInterceptor verifies — and redirects to the
// post-login destination. On failure it bounces back to /login with a
// stable error code in the query string so the page can render a message
// without leaking which half of the credential pair was wrong.
//
// The reverse proxy (Caddy in the compose stack, vite in dev) forwards
// /auth/* to this server, so the form post and the resulting Set-Cookie
// are first-party with the rest of the app.
func mountPasswordAuth(mux *http.ServeMux, app *App, logger *slog.Logger, sessionTTL time.Duration) {
	mux.HandleFunc("POST /auth/password", passwordAuthHandler(app, logger, sessionTTL))

	logger.Info("password auth endpoint mounted", "path", "/auth/password")
}

func passwordAuthHandler(app *App, logger *slog.Logger, sessionTTL time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		identifier := strings.TrimSpace(strings.ToLower(r.PostFormValue("identifier")))
		password := r.PostFormValue("password")
		dest := safeRedirect(r.PostFormValue("redirect"))

		if identifier == "" || password == "" {
			redirectLoginError(w, r, "missing_credentials")
			return
		}

		user, errCode := passwordAuthVerify(r.Context(), app, logger, identifier, password)
		if errCode != "" {
			redirectLoginError(w, r, errCode)
			return
		}
		if redirectPasswordChange(w, r, app, logger, user, dest) || redirectSecondFactor(w, r, app, logger, user, dest) {
			return
		}

		// Issue the session. Plaintext token goes in the cookie; only its
		// SHA-256 is persisted — mirrors the OIDC issueSession path.
		token, err := randomSessionToken()
		if err != nil {
			logger.Error("password login: token generation failed", "error", err)
			redirectLoginError(w, r, "server_error")
			return
		}
		hash := sha256.Sum256([]byte(token))
		now := time.Now()
		expiresAt := now.Add(sessionTTL)

		if _, err := app.Sessions.Create(r.Context(), domain.Session{
			UserID:           user.ID,
			TokenHash:        hash[:],
			AuthMethod:       domain.SessionAuthPassword,
			UserAgentSummary: truncateUA(r.UserAgent()),
			CreatedAt:        now,
			LastSeenAt:       now,
			ExpiresAt:        expiresAt,
		}); err != nil {
			logger.Error("password login: create session failed", "error", err, "user_id", user.ID)
			redirectLoginError(w, r, "server_error")
			return
		}

		setSessionCookie(w, r, token, expiresAt)
		logger.Info("password login succeeded", "user_id", user.ID, "username", user.Username)
		http.Redirect(w, r, dest, http.StatusSeeOther) //nolint:gosec // G710: dest is constrained to a local path by safeRedirect
	}
}

// passwordAuthVerify checks the credentials; a non-empty result is the login error code.
func passwordAuthVerify(ctx context.Context, app *App, logger *slog.Logger, identifier, password string) (domain.User, string) {
	// Combined lookup: resolves either a username or an email and
	// returns the user plus its credentials. A miss still runs an
	// Argon2id round below so present/absent users take ~equal time.
	user, cred, err := app.Users.GetCredentialsByLogin(ctx, identifier)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			_, _ = app.PasswordHasher.Verify(password, "")
			return domain.User{}, "invalid_credentials"
		}
		logger.Error("password login: credentials lookup failed", "error", err)
		return domain.User{}, "server_error"
	}

	ok, err := app.PasswordHasher.Verify(password, cred.PasswordHash)
	if err != nil {
		logger.Error("password login: verify failed", "error", err, "user_id", user.ID)
		return domain.User{}, "server_error"
	}
	if !ok {
		return domain.User{}, "invalid_credentials"
	}
	if !user.IsActive() {
		return domain.User{}, "account_inactive"
	}
	return user, ""
}

// safeRedirect constrains the post-login destination to a local path so
// the redirect form field can't be abused as an open redirect. Anything
// that isn't a single-slash-rooted relative path collapses to "/".
func safeRedirect(dest string) string {
	// "//host" and "/\host" are protocol-relative URLs to browsers.
	if !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") || strings.HasPrefix(dest, "/\\") {
		return "/"
	}
	// Browsers treat "\" as "/" and strip tabs/newlines, so "/\t/host" would escape.
	if strings.ContainsFunc(dest, func(c rune) bool { return c == '\\' || c < 0x20 || c == 0x7f }) {
		return "/"
	}
	return dest
}

func redirectLoginError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?error="+code, http.StatusSeeOther) //nolint:gosec // G710: fixed same-origin path; code is a server-chosen error code
}

func randomSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// truncateUA bounds a user agent to what the session and audit rows keep.
func truncateUA(s string) string {
	const n = 200
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// redirectSecondFactor hands the browser a challenge instead of a session
// when the user requires a passkey after the password; reports whether it did.
func redirectSecondFactor(w http.ResponseWriter, r *http.Request, app *App, logger *slog.Logger, user domain.User, dest string) bool {
	if !app.SecondFactor.Required(user) {
		return false
	}
	token, err := app.SecondFactor.Begin(r.Context(), user.ID)
	if err != nil {
		logger.Error("password login: begin second factor failed", "error", err, "user_id", user.ID)
		redirectLoginError(w, r, "server_error")
		return true
	}
	http.Redirect(w, r, "/login?challenge="+url.QueryEscape(token)+"&next="+url.QueryEscape(dest), http.StatusSeeOther) //nolint:gosec // G710: fixed same-origin path
	return true
}

// redirectPasswordChange hands the browser a password-change challenge
// instead of a session when the user must replace a temporary password;
// reports whether it did. The new password then completes the sign-in via
// POST /auth/password/required, which runs the second factor if needed.
func redirectPasswordChange(w http.ResponseWriter, r *http.Request, app *App, logger *slog.Logger, user domain.User, dest string) bool {
	if !app.PasswordChange.Required(user) {
		return false
	}
	token, err := app.PasswordChange.Begin(r.Context(), user.ID)
	if err != nil {
		logger.Error("password login: begin password change failed", "error", err, "user_id", user.ID)
		redirectLoginError(w, r, "server_error")
		return true
	}
	http.Redirect(w, r, "/login?set_password="+url.QueryEscape(token)+"&next="+url.QueryEscape(dest), http.StatusSeeOther) //nolint:gosec // G710: fixed same-origin path
	return true
}
