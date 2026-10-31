package rest

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
)

// acctIsHTTPS reports whether the request arrived over TLS, directly or via
// a reverse proxy that set X-Forwarded-Proto=https.
func acctIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// acctCookieSecure is the Secure attribute: always behind an https public URL,
// otherwise per request so plain-http dev keeps its cookies.
func acctCookieSecure(r *http.Request, always bool) bool {
	return always || acctIsHTTPS(r)
}

// acctSetSessionCookie sets the session cookie; Max-Age wins over a wrong client clock, Expires is the fallback.
func acctSetSessionCookie(w http.ResponseWriter, r *http.Request, secure bool, token string, expiresAt time.Time) {
	maxAge := max(int(time.Until(expiresAt).Seconds()), 0)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   acctCookieSecure(r, secure),
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
		MaxAge:   maxAge,
	})
}

func acctClearSessionCookie(w http.ResponseWriter, r *http.Request, secure bool) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   acctCookieSecure(r, secure),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// acctIssueSession opens a session for the user and sets its cookie.
func acctIssueSession(ctx context.Context, w http.ResponseWriter, r *http.Request, sessions *authuc.Sessions, secure bool, userID domain.UserID, method domain.SessionAuthMethod) error {
	s, err := sessions.Issue(ctx, userID, method, r.UserAgent())
	if err != nil {
		return err
	}
	acctSetSessionCookie(w, r, secure, s.Token, s.ExpiresAt)
	return nil
}

// acctSafeRedirect constrains a post-login destination to a local path so it
// can't be abused as an open redirect; anything else collapses to "/".
func acctSafeRedirect(dest string) string {
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

func acctRedirectLoginError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?error="+code, http.StatusSeeOther) //nolint:gosec // G710: fixed same-origin path; code is a server-chosen error code
}

// acctRedirectChallenge sends the browser back to /login with a challenge token.
func acctRedirectChallenge(w http.ResponseWriter, r *http.Request, param, token, dest string) {
	http.Redirect(w, r, "/login?"+param+"="+url.QueryEscape(token)+"&next="+url.QueryEscape(dest), http.StatusSeeOther) //nolint:gosec // G710: fixed same-origin path
}

func acctFmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// acctSessionToken is the caller's session token: the cookie, else a session bearer.
func acctSessionToken(r *http.Request) string {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		return c.Value
	}
	if tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); strings.HasPrefix(tok, domain.SessionTokenPrefix) {
		return tok
	}
	return ""
}
