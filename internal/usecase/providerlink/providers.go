// Package providerlink links provider accounts (Strava, Garmin, ...) to
// users: connection CRUD, the OAuth connect round-trip, credential-based
// accounts, the account-scoped jobs (gear sync, health import) and the
// token/credential lookups provider workers make.
package providerlink

import (
	"strings"
)

// Provider is a provider's OAuth2 endpoint knowledge (URLs + scopes). The
// credentials are per connection: a user can create several connections for
// one provider, each with its own client_id/client_secret.
type Provider struct {
	Provider     string
	DisplayName  string
	AuthorizeURL string
	TokenURL     string
	Scopes       string
	// ActivityURLTemplate deep-links the original activity; "{id}" is the
	// provider's external id. Empty when there is no public activity URL.
	ActivityURLTemplate string
}

// OAuth reports whether the provider links accounts via an OAuth authorize
// round-trip (vs. credential-based providers like Garmin).
func (p Provider) OAuth() bool { return p.AuthorizeURL != "" }

// ActivityURL is the provider's deep link for an activity, or "".
func (p Provider) ActivityURL(externalID string) string {
	if p.ActivityURLTemplate == "" || externalID == "" {
		return ""
	}
	return strings.ReplaceAll(p.ActivityURLTemplate, "{id}", externalID)
}

// ScopeList splits the comma-separated scopes.
func (p Provider) ScopeList() []string {
	if p.Scopes == "" {
		return []string{}
	}
	parts := strings.Split(p.Scopes, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// Providers are the connectable providers by id.
var Providers = map[string]Provider{
	"strava": { //nolint:gosec // G101: public OAuth endpoint URLs, no credentials
		Provider:            "strava",
		DisplayName:         "Strava",
		AuthorizeURL:        "https://www.strava.com/oauth/authorize",
		TokenURL:            "https://www.strava.com/oauth/token",
		Scopes:              "read,activity:read_all",
		ActivityURLTemplate: "https://www.strava.com/activities/{id}",
	},
	// Credential-based: client_id/client_secret hold the Garmin Connect
	// email/password and the account is created with the connection.
	"garmin": {
		Provider:            "garmin",
		DisplayName:         "Garmin",
		ActivityURLTemplate: "https://connect.garmin.com/modern/activity/{id}",
	},
}
