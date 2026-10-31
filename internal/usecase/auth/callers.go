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
	PATs     port.PATRepo         // optional
	OAuth    port.OAuthServerRepo // optional
	Users    port.UserRepo        // role gates; optional
	Now      func() time.Time
}

// Credential is what a request presented.
type Credential struct {
	Cookie   string // session cookie value
	Bearer   string // Authorization: Bearer value
	Mutating bool   // the request changes state: an OAuth token then needs a write scope
	NoOAuth  bool   // refuse OAuth access tokens (e.g. approving a device grant)
}

func (c *Callers) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now()
}

// Resolve returns the user behind cred, trying cookie, bearer session, PAT
// and OAuth token in that order.
func (c *Callers) Resolve(ctx context.Context, cred Credential) (domain.UserID, bool) {
	if cred.Cookie != "" {
		if uid, ok := c.session(ctx, cred.Cookie); ok {
			return uid, true
		}
	}
	tok := cred.Bearer
	switch {
	case strings.HasPrefix(tok, domain.SessionTokenPrefix):
		return c.session(ctx, tok)
	case strings.HasPrefix(tok, domain.PATTokenPrefix):
		return c.pat(ctx, tok)
	case strings.HasPrefix(tok, domain.OAuthAccessTokenPrefix) && !cred.NoOAuth:
		return c.oauth(ctx, tok, cred.Mutating)
	}
	return domain.UserID{}, false
}

func (c *Callers) session(ctx context.Context, tok string) (domain.UserID, bool) {
	if c.Sessions == nil {
		return domain.UserID{}, false
	}
	hash := sha256.Sum256([]byte(tok))
	sess, err := c.Sessions.GetByTokenHash(ctx, hash[:])
	if err != nil || !sess.IsActive(c.now()) {
		return domain.UserID{}, false
	}
	return sess.UserID, true
}

func (c *Callers) pat(ctx context.Context, tok string) (domain.UserID, bool) {
	if c.PATs == nil {
		return domain.UserID{}, false
	}
	hash := sha256.Sum256([]byte(tok))
	pat, err := c.PATs.FindByTokenHash(ctx, hash[:])
	if err != nil || !pat.IsValidAt(c.now()) {
		return domain.UserID{}, false
	}
	_ = c.PATs.TouchLastUsed(ctx, pat.ID)
	return pat.UserID, true
}

// oauth admits a read-only token only to non-mutating requests.
func (c *Callers) oauth(ctx context.Context, tok string, mutating bool) (domain.UserID, bool) {
	if c.OAuth == nil {
		return domain.UserID{}, false
	}
	hash := sha256.Sum256([]byte(tok))
	at, err := c.OAuth.FindAccessToken(ctx, hash[:])
	if err != nil || !at.IsValidAt(c.now()) {
		return domain.UserID{}, false
	}
	if mutating && !domain.ScopesHaveAnyWrite(at.Scope) {
		return domain.UserID{}, false
	}
	return at.UserID, true
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
