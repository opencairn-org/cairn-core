package domain

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// ProviderMeta is what a provider's worker declares about connecting the
// provider: display, auth method, OAuth endpoints and the connect-form
// fields. It arrives in the worker manifest (provider_meta) and is bound to
// the first package that published it, like a pool name (docs/adr/0006).
// Core keeps no compiled-in provider knowledge.
type ProviderMeta struct {
	ID      string `json:"-"` // provider id; the manifest's provider
	Package string `json:"-"` // bound package, "" for seeded rows

	DisplayName  string `json:"display_name"`
	Description  string `json:"description,omitempty"`
	SetupDocsURL string `json:"setup_docs_url,omitempty"`
	// ActivityURLTemplate deep-links the original activity; "{id}" is the
	// provider's external id. Empty when there is no public activity URL.
	ActivityURLTemplate string `json:"activity_url_template,omitempty"`
	// AuthMethod is AuthMethodOAuth2 or AuthMethodCredentials.
	AuthMethod string         `json:"auth_method"`
	OAuth2     *ProviderOAuth `json:"oauth,omitempty"`
	// ConfigFields label the connection's two credential slots,
	// client_id and client_secret, in the connect form.
	ConfigFields []ProviderField `json:"config_fields,omitempty"`
	UpdatedAt    time.Time       `json:"-"`
}

// Auth methods a provider connects with.
const (
	AuthMethodOAuth2      = "oauth2"
	AuthMethodCredentials = "credentials"
)

// ProviderOAuth is the authorize side of an OAuth2 provider; the code
// exchange itself runs in the worker (cairn.oauth.<provider>.<pool>.exchange).
type ProviderOAuth struct {
	AuthorizeURL string   `json:"authorize_url"`
	Scopes       []string `json:"scopes,omitempty"`
	// ScopeSeparator joins Scopes in the authorize URL; " " unless the
	// provider deviates (Strava uses ",").
	ScopeSeparator string `json:"scope_separator,omitempty"`
	PKCE           bool   `json:"pkce,omitempty"`
	// AuthorizeParams are extra authorize query parameters
	// (approval_prompt=force, access_type=offline, ...).
	AuthorizeParams map[string]string `json:"authorize_params,omitempty"`
}

// ProviderField labels one credential slot in the connect form.
type ProviderField struct {
	Key         string `json:"key"` // ConfigFieldClientID | ConfigFieldClientSecret
	Label       string `json:"label"`
	Help        string `json:"help,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
}

// The connection's credential slots.
const (
	ConfigFieldClientID     = "client_id"
	ConfigFieldClientSecret = "client_secret"
)

// ErrProviderPackageChanged: another package published meta for a provider
// already bound to one.
var ErrProviderPackageChanged = errors.New("provider meta is bound to another package")

// OAuth reports whether the provider links accounts via an OAuth authorize
// round-trip (vs. credential-based providers like Garmin).
func (p ProviderMeta) OAuth() bool { return p.AuthMethod == AuthMethodOAuth2 }

// ActivityURL is the provider's deep link for an activity, or "".
func (p ProviderMeta) ActivityURL(externalID string) string {
	if p.ActivityURLTemplate == "" || externalID == "" {
		return ""
	}
	return strings.ReplaceAll(p.ActivityURLTemplate, "{id}", url.PathEscape(externalID))
}

// ScopeString joins the scopes the way the provider's authorize URL wants.
func (p ProviderMeta) ScopeString() string {
	if p.OAuth2 == nil {
		return ""
	}
	sep := p.OAuth2.ScopeSeparator
	if sep == "" {
		sep = " "
	}
	return strings.Join(p.OAuth2.Scopes, sep)
}

// Field returns the label set for a credential slot, with defaults.
func (p ProviderMeta) Field(key string) ProviderField {
	for _, f := range p.ConfigFields {
		if f.Key == key {
			return f
		}
	}
	switch key {
	case ConfigFieldClientSecret:
		return ProviderField{Key: key, Label: "Client secret", Secret: true}
	default:
		return ProviderField{Key: key, Label: "Client ID"}
	}
}

// authorizeReserved are the query parameters core sets itself.
var authorizeReserved = map[string]bool{
	"client_id": true, "redirect_uri": true, "response_type": true, "scope": true,
	"state": true, "code_challenge": true, "code_challenge_method": true,
}

// Validate checks a manifest's provider meta before it is stored.
func (p ProviderMeta) Validate() error {
	if !ValidProviderID(p.ID) {
		return fmt.Errorf("provider id %q", p.ID)
	}
	if p.DisplayName == "" || len(p.DisplayName) > 64 {
		return errors.New("display_name: 1–64 chars")
	}
	if len(p.Description) > 500 {
		return errors.New("description: at most 500 chars")
	}
	if err := checkHTTPSURL("setup_docs_url", p.SetupDocsURL, false); err != nil {
		return err
	}
	if err := checkHTTPSURL("activity_url_template", p.ActivityURLTemplate, false); err != nil {
		return err
	}
	if p.ActivityURLTemplate != "" && !strings.Contains(p.ActivityURLTemplate, "{id}") {
		return errors.New("activity_url_template: must contain {id}")
	}
	switch p.AuthMethod {
	case AuthMethodOAuth2:
		if p.OAuth2 == nil {
			return errors.New("oauth: required for auth_method oauth2")
		}
		if err := p.OAuth2.validate(); err != nil {
			return err
		}
	case AuthMethodCredentials:
		if p.OAuth2 != nil {
			return errors.New("oauth: not allowed for auth_method credentials")
		}
	default:
		return fmt.Errorf("auth_method %q", p.AuthMethod)
	}
	return validateProviderFields(p.ConfigFields)
}

func (o ProviderOAuth) validate() error {
	if err := checkHTTPSURL("oauth.authorize_url", o.AuthorizeURL, true); err != nil {
		return err
	}
	if len(o.Scopes) > 32 {
		return errors.New("oauth.scopes: at most 32")
	}
	for _, s := range o.Scopes {
		if s == "" || len(s) > 128 {
			return errors.New("oauth.scopes: each 1–128 chars")
		}
	}
	if len(o.ScopeSeparator) > 1 {
		return errors.New("oauth.scope_separator: one char")
	}
	if len(o.AuthorizeParams) > 16 {
		return errors.New("oauth.authorize_params: at most 16")
	}
	for k, v := range o.AuthorizeParams {
		if k == "" || len(k) > 64 || len(v) > 256 {
			return errors.New("oauth.authorize_params: key 1–64, value at most 256 chars")
		}
		if authorizeReserved[k] {
			return fmt.Errorf("oauth.authorize_params: %q is set by core", k)
		}
	}
	return nil
}

func validateProviderFields(fields []ProviderField) error {
	if len(fields) > 2 {
		return errors.New("config_fields: at most client_id and client_secret")
	}
	seen := map[string]bool{}
	for _, f := range fields {
		if f.Key != ConfigFieldClientID && f.Key != ConfigFieldClientSecret {
			return fmt.Errorf("config_fields: unknown key %q", f.Key)
		}
		if seen[f.Key] {
			return fmt.Errorf("config_fields: %q twice", f.Key)
		}
		seen[f.Key] = true
		if f.Label == "" || len(f.Label) > 64 || len(f.Help) > 500 || len(f.Placeholder) > 128 {
			return fmt.Errorf("config_fields.%s: label 1–64, help at most 500, placeholder at most 128 chars", f.Key)
		}
	}
	return nil
}

// checkHTTPSURL accepts "" (unless required) or an absolute https URL; plain
// http only on a loopback host (dev stacks, tests).
func checkHTTPSURL(field, raw string, required bool) error {
	if raw == "" {
		if required {
			return fmt.Errorf("%s: required", field)
		}
		return nil
	}
	if len(raw) > 512 {
		return fmt.Errorf("%s: at most 512 chars", field)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: not an absolute URL", field)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("%s: must be https", field)
}

// OAuthExchangeSubject is the request/reply subject a pool answers the
// authorization-code exchange on.
func OAuthExchangeSubject(provider, workerName string) string {
	return "cairn.oauth." + provider + "." + workerName + ".exchange"
}
