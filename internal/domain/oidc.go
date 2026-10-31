package domain

import "time"

// ---------------------------------------------------------------------------
// OIDCProvider
//
// A configured Identity Provider. Providers are declared ENTIRELY in the
// environment (CAIRN_OIDC_PROVIDERS + CAIRN_OIDC_<ID>_*) and loaded into an
// in-memory map at start-up — there is no database row and no admin-UI editing.
// The admin UI shows this config read-only.
//
// The flow stays a confidential authorization-code exchange (client secret),
// not a public client. ClientSecret is the operator-managed plaintext from the
// environment (never persisted, so there's nothing to encrypt at rest).
// ---------------------------------------------------------------------------

type OIDCProvider struct {
	// ID is the stable provider key from CAIRN_OIDC_PROVIDERS (e.g. "google").
	// It appears in the login button, the /auth/oidc/{id}/* URLs, and
	// linked_identities.provider — so it must not change once users have linked.
	ID   string
	Name string // human-readable label on the login button; defaults to ID

	IssuerURL    string
	ClientID     string
	ClientSecret string   // confidential client secret (from env, never stored)
	Scopes       []string // defaults to [openid, email, profile]

	// Audience is the expected `aud` claim; defaults to ClientID.
	// SkipAudienceCheck disables the aud check for IdPs that omit it.
	Audience          string
	SkipAudienceCheck bool

	UsePKCE bool // adds PKCE to the code flow (recommended; default true)

	// AutoProvision creates a Cairn user on first sign-in (default true).
	// AutoProvisionRole is the role for those users (default "user").
	AutoProvision     bool
	AutoProvisionRole UserRole
}

// EffectiveAudience returns Audience, falling back to ClientID.
func (p OIDCProvider) EffectiveAudience() string {
	if p.Audience != "" {
		return p.Audience
	}
	return p.ClientID
}

// Standard OIDC claim keys used to map an ID token onto a Cairn user. Claim
// mapping is no longer configurable per-provider — the spec-standard claims
// cover the supported IdPs (PocketID, Google, Apple, Authentik, Keycloak...).
const (
	ClaimEmail         = "email"
	ClaimEmailVerified = "email_verified"
	ClaimUsername      = "preferred_username"
	ClaimDisplayName   = "name"
)

// OIDCIDToken is a signature-, issuer- and audience-verified id_token. Claims
// is the full claim set (persisted as LinkedIdentity.LastClaims).
type OIDCIDToken struct {
	Subject string
	Nonce   string
	Claims  map[string]any
}

// ---------------------------------------------------------------------------
// LinkedIdentity
//
// One row per (provider, subject) pair. The same Cairn user can have multiple
// identities across providers. Provider is the OIDCProvider.ID string (the env
// key) — there is no oidc_clients table to key on anymore.
// ---------------------------------------------------------------------------

type LinkedIdentity struct {
	ID         LinkedIdentityID
	UserID     UserID
	Provider   string // OIDCProvider.ID, e.g. "google"
	Subject    string
	Email      string
	LastClaims map[string]any
	LinkedAt   time.Time
	LastUsedAt *time.Time
}
