package domain

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// OAuth 2.1 authorization-server domain
//
// Cairn is an OAuth 2.1 authorization server for native apps, third-party
// clients and MCP agents. These are pure types + scope/PKCE helpers; storage
// lives in port.OAuthServerRepo and the postgres adapter, the flow in
// cmd/server/oauth_as.go.
// ---------------------------------------------------------------------------

// Scope catalog. Access tokens carry a space-separated subset; the API and the
// MCP server enforce them. Read-only is the safe default for agents.
const (
	ScopeActivitiesRead     = "activities:read"
	ScopeActivitiesWrite    = "activities:write"
	ScopeProfileRead        = "profile:read"
	ScopeProfileWrite       = "profile:write"
	ScopeSocialRead         = "social:read"
	ScopeSocialWrite        = "social:write"
	ScopeSegmentsRead       = "segments:read"
	ScopeSegmentsWrite      = "segments:write"
	ScopeTrainingRead       = "training:read" // training load, best efforts, health and athlete metrics
	ScopeTrainingWrite      = "training:write"
	ScopeGearRead           = "gear:read"
	ScopeGearWrite          = "gear:write"
	ScopeNotificationsRead  = "notifications:read"
	ScopeNotificationsWrite = "notifications:write"
	ScopeConnectionsRead    = "connections:read" // provider connections and sync status
	ScopeConnectionsWrite   = "connections:write"
)

// SupportedScopes is the full catalog advertised in AS metadata. Every family
// has a read and a write scope; docs/api-scopes.md maps RPCs to them.
var SupportedScopes = []string{
	ScopeActivitiesRead,
	ScopeActivitiesWrite,
	ScopeProfileRead,
	ScopeProfileWrite,
	ScopeSocialRead,
	ScopeSocialWrite,
	ScopeSegmentsRead,
	ScopeSegmentsWrite,
	ScopeTrainingRead,
	ScopeTrainingWrite,
	ScopeGearRead,
	ScopeGearWrite,
	ScopeNotificationsRead,
	ScopeNotificationsWrite,
	ScopeConnectionsRead,
	ScopeConnectionsWrite,
}

// ReadOnlyScopes is the default grant for agents (no write capability).
var ReadOnlyScopes = []string{
	ScopeActivitiesRead,
	ScopeProfileRead,
	ScopeSocialRead,
	ScopeSegmentsRead,
	ScopeTrainingRead,
	ScopeGearRead,
	ScopeNotificationsRead,
	ScopeConnectionsRead,
}

// ScopeFamily returns the part before the colon ("activities" for
// "activities:read"); "" when s has no family.
func ScopeFamily(s string) string {
	if i := strings.IndexByte(s, ':'); i > 0 {
		return s[:i]
	}
	return ""
}

// ScopesGrant reports whether scopes allow an operation on family: a read
// needs family:read or family:write, a write needs family:write.
func ScopesGrant(scopes []string, family string, write bool) bool {
	for _, s := range scopes {
		if s == family+":write" {
			return true
		}
		if !write && s == family+":read" {
			return true
		}
	}
	return false
}

// ScopeSupported reports whether s is a known scope.
func ScopeSupported(s string) bool {
	for _, x := range SupportedScopes {
		if x == s {
			return true
		}
	}
	return false
}

// IsWriteScope reports whether s grants mutation.
func IsWriteScope(s string) bool { return strings.HasSuffix(s, ":write") }

// ParseScopes splits a space-separated scope string, dropping blanks.
func ParseScopes(s string) []string {
	var out []string
	out = append(out, strings.Fields(s)...)
	return out
}

// JoinScopes renders scopes as the space-separated wire form.
func JoinScopes(scopes []string) string { return strings.Join(scopes, " ") }

// ScopesContain reports whether the space-separated scope string grants want.
func ScopesContain(scope, want string) bool {
	for _, s := range strings.Fields(scope) {
		if s == want {
			return true
		}
	}
	return false
}

// ScopesHaveAnyWrite reports whether the space-separated scope string grants
// any mutation scope.
func ScopesHaveAnyWrite(scope string) bool {
	for _, s := range strings.Fields(scope) {
		if IsWriteScope(s) {
			return true
		}
	}
	return false
}

// FilterRequestedScopes intersects the requested scopes with what the client is
// allowed and what Cairn supports, preserving catalog order and de-duping. An
// empty request defaults to the client's allowed scopes.
func FilterRequestedScopes(requested, clientAllowed []string) []string {
	allowed := map[string]bool{}
	for _, s := range clientAllowed {
		allowed[s] = true
	}
	want := map[string]bool{}
	if len(requested) == 0 {
		for _, s := range clientAllowed {
			want[s] = true
		}
	} else {
		for _, s := range requested {
			want[s] = true
		}
	}
	var out []string
	for _, s := range SupportedScopes {
		if want[s] && (len(clientAllowed) == 0 || allowed[s]) {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// OAuthClient is a registered OAuth client. ClientID is the public identifier.
// A public client (SecretHash == nil) must use PKCE; a confidential client
// authenticates at the token endpoint with its secret.
type OAuthClient struct {
	ClientID                string
	SecretHash              []byte // nil => public client (PKCE-only)
	Name                    string
	RedirectURIs            []string
	GrantTypes              []string
	Scopes                  []string // scopes the client may request
	TokenEndpointAuthMethod string   // none | client_secret_basic | client_secret_post
	IsDynamic               bool
	// SoftwareID / SoftwareVersion identify the application behind a dynamic
	// registration (RFC 7591 §2), e.g. "org.opencairn.cairn" / "1.4.0", so an
	// "authorized apps" view can show which app a client row belongs to.
	SoftwareID      string
	SoftwareVersion string
	CreatedBy       *UserID
	CreatedAt       time.Time
}

// IsPublic reports whether the client has no secret (PKCE-only).
func (c OAuthClient) IsPublic() bool { return len(c.SecretHash) == 0 }

// AllowsRedirectURI reports whether uri matches a registered redirect.
func (c OAuthClient) AllowsRedirectURI(uri string) bool {
	_, ok := c.RegisteredRedirectURI(uri)
	return ok
}

// RegisteredRedirectURI returns the redirect target for uri: the stored value on
// an exact match, so callers redirect to registered input rather than request
// input. Loopback redirects (RFC 8252 §7.3: http scheme, host 127.0.0.1 or
// [::1]) match a registered loopback redirect regardless of port, and the
// target is then the requested uri, because a native app listens on whatever
// port it could bind. "localhost" and custom schemes stay exact.
func (c OAuthClient) RegisteredRedirectURI(uri string) (string, bool) {
	if uri == "" {
		return "", false
	}
	for _, u := range c.RedirectURIs {
		if u == uri && u != "" {
			return u, true
		}
	}
	req, ok := loopbackRedirect(uri)
	if !ok {
		return "", false
	}
	for _, u := range c.RedirectURIs {
		reg, ok := loopbackRedirect(u)
		if ok && reg.Hostname() == req.Hostname() && reg.Path == req.Path && reg.RawQuery == req.RawQuery {
			return uri, true
		}
	}
	return "", false
}

// loopbackRedirect parses uri when it is an http loopback-IP redirect.
func loopbackRedirect(uri string) (*url.URL, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "http" || u.Fragment != "" || u.User != nil {
		return nil, false
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1":
		return u, true
	}
	return nil, false
}

// AllowsGrant reports whether the client may use grant type g.
func (c OAuthClient) AllowsGrant(g string) bool {
	for _, x := range c.GrantTypes {
		if x == g {
			return true
		}
	}
	return false
}

// VerifySecret constant-time compares a presented secret against the stored
// hash. Always false for public clients.
func (c OAuthClient) VerifySecret(secret string) bool {
	if c.IsPublic() {
		return false
	}
	h := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(h[:], c.SecretHash) == 1
}

// ---------------------------------------------------------------------------
// Authorization code
// ---------------------------------------------------------------------------

// OAuthAuthorizationCode is a one-time code exchanged for tokens. Only its hash
// is stored.
type OAuthAuthorizationCode struct {
	ClientID            string
	UserID              UserID
	RedirectURI         string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	ExpiresAt           time.Time
	ConsumedAt          *time.Time
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

// OAuthAccessToken authorizes API/MCP calls as UserID, bounded by Scope.
type OAuthAccessToken struct {
	ClientID  string
	UserID    UserID
	Scope     string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// IsValidAt reports whether the token authenticates at now.
func (t OAuthAccessToken) IsValidAt(now time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	return now.Before(t.ExpiresAt)
}

// HasScope reports whether the token grants want.
func (t OAuthAccessToken) HasScope(want string) bool { return ScopesContain(t.Scope, want) }

// OAuthRefreshToken mints fresh access tokens without re-consent.
type OAuthRefreshToken struct {
	ClientID  string
	UserID    UserID
	Scope     string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// IsValidAt reports whether the refresh token is usable at now.
func (t OAuthRefreshToken) IsValidAt(now time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	return now.Before(t.ExpiresAt)
}

// ---------------------------------------------------------------------------
// PKCE
// ---------------------------------------------------------------------------

// VerifyPKCE checks a code_verifier against the stored code_challenge. Only the
// S256 method is accepted (OAuth 2.1 forbids "plain").
func VerifyPKCE(verifier, challenge, method string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	if method != "" && method != "S256" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// SortedScopeString normalizes a scope slice to a stable space-separated form.
func SortedScopeString(scopes []string) string {
	cp := append([]string(nil), scopes...)
	sort.Strings(cp)
	return strings.Join(cp, " ")
}

// OAuthGrant is one client's standing access to a user: what the user sees
// on the "authorized apps" page and can revoke as a whole.
type OAuthGrant struct {
	ClientID        string
	ClientName      string
	SoftwareID      string
	SoftwareVersion string
	// Scopes is the union of the client's live token scopes.
	Scopes []string
	// GrantedAt is the oldest live token; LastIssuedAt the newest.
	GrantedAt    time.Time
	LastIssuedAt time.Time
	ActiveTokens int
}
