package auth

import (
	"context"
	"crypto/sha256"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Callers resolves the credential a request presents to the calling user:
// a session (cookie or cairn_ses_ bearer), a personal access token or an
// OAuth access token. Every primary adapter authenticates through it.
type Callers struct {
	Sessions port.SessionRepo
	PATs     port.PATRepo          // optional
	OAuth    port.OAuthTokenLookup // optional
	Users    port.UserReader       // role gates; optional
	Now      func() time.Time
}

// Credential is what a request presented.
type Credential struct {
	Cookie   string // session cookie value
	Bearer   string // Authorization: Bearer value
	Mutating bool   // the request changes state: an OAuth token then needs a write scope
	NoOAuth  bool   // refuse OAuth access tokens (e.g. approving a device grant)
	// TouchSession records last-seen on a session hit.
	TouchSession bool
}

func (c *Callers) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now()
}

// CallerKind says which credential authenticated a caller.
type CallerKind int

const (
	CallerSession CallerKind = iota + 1 // cookie or cairn_ses_ bearer
	CallerPAT
	CallerOAuth
)

// Caller is the authenticated principal behind a credential.
type Caller struct {
	UserID    domain.UserID
	Kind      CallerKind
	SessionID domain.SessionID // CallerSession only
	Scopes    []string         // CallerOAuth only; never nil then
}

// Resolve returns the user behind cred, trying cookie, bearer session, PAT
// and OAuth token in that order.
func (c *Callers) Resolve(ctx context.Context, cred Credential) (domain.UserID, bool) {
	caller, ok := c.Authenticate(ctx, cred)
	return caller.UserID, ok
}

// Authenticate is Resolve returning the whole principal.
func (c *Callers) Authenticate(ctx context.Context, cred Credential) (Caller, bool) {
	if cred.Cookie != "" {
		if caller, ok := c.session(ctx, cred.Cookie, cred.TouchSession); ok {
			return caller, true
		}
	}
	tok := cred.Bearer
	switch {
	case strings.HasPrefix(tok, domain.SessionTokenPrefix):
		return c.session(ctx, tok, cred.TouchSession)
	case strings.HasPrefix(tok, domain.PATTokenPrefix):
		return c.pat(ctx, tok)
	case strings.HasPrefix(tok, domain.OAuthAccessTokenPrefix) && !cred.NoOAuth:
		return c.oauth(ctx, tok, cred.Mutating)
	}
	return Caller{}, false
}

func (c *Callers) session(ctx context.Context, tok string, touch bool) (Caller, bool) {
	if c.Sessions == nil {
		return Caller{}, false
	}
	hash := sha256.Sum256([]byte(tok))
	sess, err := c.Sessions.GetByTokenHash(ctx, hash[:])
	if err != nil || !sess.IsActive(c.now()) {
		return Caller{}, false
	}
	if touch {
		_ = c.Sessions.TouchLastSeen(ctx, sess.ID)
	}
	return Caller{UserID: sess.UserID, Kind: CallerSession, SessionID: sess.ID}, true
}

func (c *Callers) pat(ctx context.Context, tok string) (Caller, bool) {
	if c.PATs == nil {
		return Caller{}, false
	}
	hash := sha256.Sum256([]byte(tok))
	pat, err := c.PATs.FindByTokenHash(ctx, hash[:])
	if err != nil || !pat.IsValidAt(c.now()) {
		return Caller{}, false
	}
	_ = c.PATs.TouchLastUsed(ctx, pat.ID)
	return Caller{UserID: pat.UserID, Kind: CallerPAT}, true
}

// oauth admits a read-only token only to non-mutating requests.
func (c *Callers) oauth(ctx context.Context, tok string, mutating bool) (Caller, bool) {
	if c.OAuth == nil {
		return Caller{}, false
	}
	hash := sha256.Sum256([]byte(tok))
	at, err := c.OAuth.FindAccessToken(ctx, hash[:])
	if err != nil || !at.IsValidAt(c.now()) {
		return Caller{}, false
	}
	if mutating && !domain.ScopesHaveAnyWrite(at.Scope) {
		return Caller{}, false
	}
	scopes := domain.ParseScopes(at.Scope)
	if scopes == nil {
		scopes = []string{} // a scopeless token must never read as unrestricted
	}
	return Caller{UserID: at.UserID, Kind: CallerOAuth, Scopes: scopes}, true
}

// WithRole loads id when allow admits their role.
func (c *Callers) WithRole(ctx context.Context, id domain.UserID, allow func(domain.UserRole) bool) (domain.User, bool) {
	if c.Users == nil {
		return domain.User{}, false
	}
	u, err := c.Users.GetUser(ctx, id)
	if err != nil || !allow(u.Role) {
		return domain.User{}, false
	}
	return u, true
}
