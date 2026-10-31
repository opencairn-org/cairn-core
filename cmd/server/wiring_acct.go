package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Cookie and e-mail plumbing for the composition root (Connect, rate limit,
// OAuth connect wiring).

const sessionCookieName = rest.SessionCookieName

// secureCookiesAlways is set at startup when PUBLIC_BASE_URL is https.
var secureCookiesAlways bool

// cookieSecure is the Secure cookie attribute: always behind an https public URL, otherwise
// per request so plain-http dev (http://localhost:8080) keeps its cookies.
func cookieSecure(r *http.Request) bool {
	return secureCookiesAlways || r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure unless the public URL and the request are plain http (dev)
		Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true, Secure: cookieSecure(r),
		SameSite: http.SameSiteLaxMode, Expires: expiresAt, MaxAge: max(int(time.Until(expiresAt).Seconds()), 0),
	})
}

// safeRedirect constrains a post-login destination to a local path.
func safeRedirect(dest string) string {
	if !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") || strings.HasPrefix(dest, "/\\") {
		return "/"
	}
	if strings.ContainsFunc(dest, func(c rune) bool { return c == '\\' || c < 0x20 || c == 0x7f }) {
		return "/"
	}
	return dest
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	return r.RemoteAddr
}

// sendVerificationEmail mails a verification link (Connect signup path).
func sendVerificationEmail(ctx context.Context, app *App, logger *slog.Logger, base string, user domain.User) error {
	err := acctVerification(app, base).Send(ctx, user)
	if err != nil {
		logger.Error("send email verification", "error", err)
	}
	return err
}
