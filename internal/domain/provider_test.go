package domain

import (
	"strings"
	"testing"
)

func validOAuthMeta() ProviderMeta {
	return ProviderMeta{
		ID: "strava", DisplayName: "Strava", AuthMethod: AuthMethodOAuth2,
		ActivityURLTemplate: "https://www.strava.com/activities/{id}",
		OAuth2: &ProviderOAuth{
			AuthorizeURL: "https://www.strava.com/oauth/authorize", Scopes: []string{"read", "activity:read_all"},
			ScopeSeparator: ",", AuthorizeParams: map[string]string{"approval_prompt": "force"},
		},
		ConfigFields: []ProviderField{{Key: "client_id", Label: "Client ID"}, {Key: "client_secret", Label: "Secret", Secret: true}},
	}
}

func TestProviderMetaValidate(t *testing.T) {
	if err := validOAuthMeta().Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	creds := ProviderMeta{ID: "garmin", DisplayName: "Garmin", AuthMethod: AuthMethodCredentials}
	if err := creds.Validate(); err != nil {
		t.Fatalf("credentials: %v", err)
	}
	dev := validOAuthMeta()
	dev.OAuth2.AuthorizeURL = "http://127.0.0.1:8080/authorize"
	if err := dev.Validate(); err != nil {
		t.Fatalf("loopback http: %v", err)
	}

	bad := map[string]func(m *ProviderMeta){
		"id":              func(m *ProviderMeta) { m.ID = "Strava" },
		"display":         func(m *ProviderMeta) { m.DisplayName = "" },
		"auth method":     func(m *ProviderMeta) { m.AuthMethod = "magic" },
		"oauth missing":   func(m *ProviderMeta) { m.OAuth2 = nil },
		"plain http":      func(m *ProviderMeta) { m.OAuth2.AuthorizeURL = "http://example.com/auth" },
		"relative url":    func(m *ProviderMeta) { m.OAuth2.AuthorizeURL = "/auth" },
		"reserved param":  func(m *ProviderMeta) { m.OAuth2.AuthorizeParams["redirect_uri"] = "x" },
		"template no id":  func(m *ProviderMeta) { m.ActivityURLTemplate = "https://x.example/a" },
		"unknown field":   func(m *ProviderMeta) { m.ConfigFields[0].Key = "token" },
		"duplicate field": func(m *ProviderMeta) { m.ConfigFields[1].Key = "client_id" },
		"long help":       func(m *ProviderMeta) { m.ConfigFields[0].Help = strings.Repeat("x", 501) },
		"creds with oauth": func(m *ProviderMeta) {
			m.AuthMethod = AuthMethodCredentials
		},
	}
	for name, mutate := range bad {
		m := validOAuthMeta()
		mutate(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestProviderMetaHelpers(t *testing.T) {
	m := validOAuthMeta()
	if !m.OAuth() || m.ScopeString() != "read,activity:read_all" {
		t.Errorf("oauth %v scopes %q", m.OAuth(), m.ScopeString())
	}
	m.OAuth2.ScopeSeparator = ""
	if m.ScopeString() != "read activity:read_all" {
		t.Errorf("default separator: %q", m.ScopeString())
	}
	if got := m.ActivityURL("12 3"); got != "https://www.strava.com/activities/12%203" {
		t.Errorf("activity url %q", got)
	}
	if m.ActivityURL("") != "" || (ProviderMeta{}).ActivityURL("1") != "" {
		t.Error("empty activity url expected")
	}
	if f := m.Field(ConfigFieldClientSecret); f.Label != "Secret" || !f.Secret {
		t.Errorf("field %+v", f)
	}
	if f := (ProviderMeta{}).Field(ConfigFieldClientID); f.Label != "Client ID" {
		t.Errorf("default field %+v", f)
	}
	if OAuthExchangeSubject("polar", "p1") != "cairn.oauth.polar.p1.exchange" {
		t.Error("exchange subject")
	}
}
