package rest

import (
	"net/http"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
)

// SessionCookieName is the long-lived browser session cookie.
const SessionCookieName = "cairn_session"

// Auth reads a request's credential and resolves it through the Callers use case.
type Auth struct {
	Callers *authuc.Callers
}

func credentialOf(r *http.Request) authuc.Credential {
	cred := authuc.Credential{
		Bearer:   strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		Mutating: r.Method != http.MethodGet && r.Method != http.MethodHead,
	}
	if c, err := r.Cookie(SessionCookieName); err == nil {
		cred.Cookie = c.Value
	}
	return cred
}

// User is the authenticated caller.
func (a *Auth) User(r *http.Request) (domain.UserID, bool) {
	if a == nil || a.Callers == nil {
		return domain.UserID{}, false
	}
	return a.Callers.Resolve(r.Context(), credentialOf(r))
}

// UserNoOAuth is User, refusing OAuth access tokens.
func (a *Auth) UserNoOAuth(r *http.Request) (domain.UserID, bool) {
	if a == nil || a.Callers == nil {
		return domain.UserID{}, false
	}
	cred := credentialOf(r)
	cred.NoOAuth = true
	return a.Callers.Resolve(r.Context(), cred)
}

// Viewer is the caller when there is one, nil for anonymous requests.
func (a *Auth) Viewer(r *http.Request) *domain.UserID {
	if uid, ok := a.User(r); ok {
		return &uid
	}
	return nil
}

// Admin is the caller when they are an admin.
func (a *Auth) Admin(r *http.Request) (domain.User, bool) {
	return a.withRole(r, func(role domain.UserRole) bool { return role == domain.UserRoleAdmin })
}

// Moderator is the caller when they may moderate (admins included).
func (a *Auth) Moderator(r *http.Request) (domain.User, bool) {
	return a.withRole(r, domain.UserRole.CanModerate)
}

func (a *Auth) withRole(r *http.Request, allow func(domain.UserRole) bool) (domain.User, bool) {
	uid, ok := a.User(r)
	if !ok {
		return domain.User{}, false
	}
	return a.Callers.WithRole(r.Context(), uid, allow)
}

// requireUser writes 401 and reports false when the request is anonymous.
func (a *Auth) requireUser(w http.ResponseWriter, r *http.Request) (domain.UserID, bool) {
	uid, ok := a.User(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
	}
	return uid, ok
}

// actor is the audit actor for an admin acting through a request.
func actor(r *http.Request, admin domain.User) audit.Actor {
	a := audit.UserActor(admin)
	a.IP = hostOnly(clientIP(r))
	a.UserAgent = truncateUA(r.UserAgent())
	return a
}
