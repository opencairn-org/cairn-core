package domain

// OAuth 2.1 token prefixes, so a resolver can tell a bearer token's kind.
const (
	OAuthAccessTokenPrefix  = "cairn_at_"
	OAuthRefreshTokenPrefix = "cairn_rt_" //nolint:gosec // G101: token prefix marker, not a credential
	OAuthAuthCodePrefix     = "cairn_ac_"
)
