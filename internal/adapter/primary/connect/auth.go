package connect

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	connectrpc "connectrpc.com/connect/v2"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// connectErrNotAuthenticated is returned when no identity is in the context.
//
// The ONLY auth source is the user id stuffed into the request context by
// SessionInterceptor after verifying the cairn_session cookie. The frontend
// (a same-origin SPA) sends that cookie automatically, so no header-based
// fallback is needed.
//
// SECURITY: a previous version also accepted an `X-Cairn-User-ID` request
// header as a fallback identity. That was an authentication bypass — the
// reverse proxy does not strip the header, so any client could impersonate
// any user by setting it. It has been removed; do not reintroduce a
// client-controlled identity header.
func connectErrNotAuthenticated() error {
	return connectrpc.NewError(connectrpc.CodeUnauthenticated, "not authenticated")
}

// connectError keeps the v1 wire message (err.Error()) and err as the local cause.
func connectError(code connectrpc.Code, err error) error {
	return connectrpc.NewError(code, err.Error()).WithCause(err)
}

// notFoundErr is the canonical mapping for repo lookups that miss.
func notFoundErr(err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		return connectError(connectrpc.CodeNotFound, err)
	}
	return connectError(connectrpc.CodeInternal, err)
}

// serverInterceptor adapts fn to a server interceptor for every stream type.
// Streams used to pass through untouched, which left any streaming RPC
// unauthenticated and unscoped; the auth chain must see them too.
func serverInterceptor(fn func(ctx context.Context, spec connectrpc.Spec, next func(context.Context) error) error) connectrpc.ServerInterceptor {
	return func(next connectrpc.ServerFunc) connectrpc.ServerFunc {
		return func(ctx context.Context, spec connectrpc.Spec, stream connectrpc.ServerStream) error {
			return fn(ctx, spec, func(ctx context.Context) error { return next(ctx, spec, stream) })
		}
	}
}

// requestHeader returns the inbound request headers of a server call.
func requestHeader(ctx context.Context) *connectrpc.Header {
	if info, ok := connectrpc.CallInfoForServerContext(ctx); ok {
		return info.RequestHeader()
	}
	return &connectrpc.Header{}
}

// ---------------------------------------------------------------------------
// SessionInterceptor
//
// A Connect-RPC interceptor that reads the cairn_session cookie, looks
// up the session row, verifies it's active, and stuffs the resolved
// user ID into the request context. Handlers then call
// userIDFromHandlerCtx which checks the context.
//
// Unauthenticated requests pass through — the interceptor doesn't deny;
// it only enriches. The handler decides whether the operation needs
// auth and emits CodeUnauthenticated when userIDFromHandlerCtx finds
// nothing.
// ---------------------------------------------------------------------------

type ctxKey int

const (
	ctxKeyUserID ctxKey = iota
	// ctxKeyScopes holds []string of OAuth scopes when the caller authenticated
	// with an OAuth access token. Absent for cookie/PAT auth, which carry the
	// user's full access (nil scopes == unrestricted).
	ctxKeyScopes
)

// patTokenPrefix marks a personal access token; oauthTokenPrefix marks an
// OAuth 2.1 access token issued by Cairn's authorization server.
const (
	patTokenPrefix   = "cairn_pat_" //nolint:gosec // G101: token prefix, not a credential
	oauthTokenPrefix = "cairn_at_"
)

// SessionInterceptor builds a connect.ServerInterceptor that resolves the caller's
// identity from either the cairn_session cookie (browser) or an
// `Authorization: Bearer cairn_pat_…` personal access token (CLI/API), stashing
// the user id in the context. Touches last-seen/last-used best-effort. `pats`
// may be nil (PAT auth then disabled).
func SessionInterceptor(sessions port.SessionRepo, pats port.PATRepo, oauth port.OAuthServerRepo, sessionCookieName string) connectrpc.ServerInterceptor {
	return serverInterceptor(func(ctx context.Context, _ connectrpc.Spec, next func(context.Context) error) error {
		if authed, ok := connectAuthenticate(ctx, requestHeader(ctx), sessions, pats, oauth, sessionCookieName); ok {
			return next(authed)
		}
		// Not authenticated; handlers decide whether to refuse.
		return next(ctx)
	})
}

// connectAuthenticate tries cookie, PAT, then OAuth token; ok reports whether one matched.
func connectAuthenticate(ctx context.Context, h *connectrpc.Header, sessions port.SessionRepo, pats port.PATRepo, oauth port.OAuthServerRepo, sessionCookieName string) (context.Context, bool) {
	// 1. Session cookie.
	if authed, ok := connectAuthSessionCookie(ctx, h, sessions, sessionCookieName); ok {
		return authed, true
	}
	// Bearer credential: PAT or OAuth access token.
	tok := strings.TrimPrefix(h.Get("Authorization"), "Bearer ")
	// 2. Personal access token (full user access).
	if pats != nil && strings.HasPrefix(tok, patTokenPrefix) {
		hash := sha256.Sum256([]byte(tok))
		if pat, err := pats.FindByTokenHash(ctx, hash[:]); err == nil && pat.IsValidAt(time.Now().UTC()) {
			_ = pats.TouchLastUsed(ctx, pat.ID)
			return context.WithValue(ctx, ctxKeyUserID, pat.UserID), true
		}
	}
	// 3. OAuth 2.1 access token (scoped access; ScopeInterceptor enforces).
	if oauth != nil && strings.HasPrefix(tok, oauthTokenPrefix) {
		hash := sha256.Sum256([]byte(tok))
		if at, err := oauth.FindAccessToken(ctx, hash[:]); err == nil && at.IsValidAt(time.Now().UTC()) {
			ctx = context.WithValue(ctx, ctxKeyUserID, at.UserID)
			ctx = context.WithValue(ctx, ctxKeyScopes, domain.ParseScopes(at.Scope))
			return ctx, true
		}
	}
	return ctx, false
}

// connectAuthSessionCookie resolves the caller from the session cookie, if any.
func connectAuthSessionCookie(ctx context.Context, h *connectrpc.Header, sessions port.SessionRepo, sessionCookieName string) (context.Context, bool) {
	if sessions == nil {
		return ctx, false
	}
	token := readSessionCookie(h.Get("Cookie"), sessionCookieName)
	if token == "" {
		return ctx, false
	}
	hash := sha256.Sum256([]byte(token))
	if sess, err := sessions.GetByTokenHash(ctx, hash[:]); err == nil && sess.IsActive(time.Now().UTC()) {
		_ = sessions.TouchLastSeen(ctx, sess.ID)
		return context.WithValue(ctx, ctxKeyUserID, sess.UserID), true
	}
	return ctx, false
}

// ScopeInterceptor enforces OAuth scopes on the Connect API. It only applies to
// callers authenticated via an OAuth access token (cookie/PAT auth carries the
// user's full access and is unaffected). A mutating procedure requires the
// token to hold at least one write scope; otherwise the call is denied. This
// keeps read-only tokens — the default for agents — truly read-only.
//
// Mount AFTER SessionInterceptor so scopes are already in ctx.
func ScopeInterceptor() connectrpc.ServerInterceptor {
	return serverInterceptor(func(ctx context.Context, spec connectrpc.Spec, next func(context.Context) error) error {
		scopes, isOAuth := ctx.Value(ctxKeyScopes).([]string)
		if !isOAuth {
			return next(ctx) // cookie/PAT: unrestricted
		}
		if isMutatingProcedure(spec.Procedure) && !hasAnyWriteScope(scopes) {
			return connectrpc.NewError(connectrpc.CodePermissionDenied,
				"this OAuth token lacks a write scope for this operation")
		}
		return next(ctx)
	})
}

// isMutatingProcedure classifies a Connect procedure ("/svc/Method") as a
// mutation unless its method name starts with a read verb.
func isMutatingProcedure(procedure string) bool {
	method := procedure
	if i := strings.LastIndexByte(procedure, '/'); i >= 0 {
		method = procedure[i+1:]
	}
	for _, verb := range []string{"Get", "List", "Search", "Watch", "Stream", "Export", "Resolve", "Check", "Count", "Lookup", "Fetch"} {
		if strings.HasPrefix(method, verb) {
			return false
		}
	}
	return true
}

func hasAnyWriteScope(scopes []string) bool {
	for _, s := range scopes {
		if domain.IsWriteScope(s) {
			return true
		}
	}
	return false
}

// userIDFromHandlerCtx is the primary helper for handlers to call. It
// reads the interceptor stash directly off the supplied ctx.
func userIDFromHandlerCtx(ctx context.Context) (domain.UserID, error) {
	if v := ctx.Value(ctxKeyUserID); v != nil {
		if id, ok := v.(domain.UserID); ok {
			return id, nil
		}
	}
	return domain.UserID{}, connectErrNotAuthenticated()
}

// RequireAdminInterceptor refuses any RPC where the resolved user
// doesn't have the admin role. Mount this on services where every RPC
// is admin-only (AdminService). Must run after SessionInterceptor in
// the chain so the user id is already in ctx.
//
// Cost: one extra UserRepo.GetUser lookup per call. Cache later if it
// shows up on traces — admin endpoints are low-volume so the join is
// fine for now.
func RequireAdminInterceptor(users port.UserRepo) connectrpc.ServerInterceptor {
	return serverInterceptor(func(ctx context.Context, _ connectrpc.Spec, next func(context.Context) error) error {
		v := ctx.Value(ctxKeyUserID)
		if v == nil {
			return connectrpc.NewError(connectrpc.CodeUnauthenticated, "authentication required")
		}
		uid, ok := v.(domain.UserID)
		if !ok {
			return connectrpc.NewError(connectrpc.CodeUnauthenticated, "invalid session")
		}
		u, err := users.GetUser(ctx, uid)
		if err != nil {
			return connectrpc.NewError(connectrpc.CodeUnauthenticated, "user not found")
		}
		if u.Role != domain.UserRoleAdmin {
			return connectrpc.NewError(connectrpc.CodePermissionDenied, "admin role required")
		}
		return next(ctx)
	})
}

// readSessionCookie parses the supplied Cookie header line for the named
// session cookie. Tolerates multiple cookies in one header and ignores
// quoting (browsers don't quote session cookie values).
func readSessionCookie(cookieHeader, name string) string {
	if cookieHeader == "" || name == "" {
		return ""
	}
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		if eq := strings.IndexByte(part, '='); eq > 0 {
			if part[:eq] == name {
				return part[eq+1:]
			}
		}
	}
	return ""
}
