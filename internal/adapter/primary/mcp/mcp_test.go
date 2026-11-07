package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/oauthserver"
)

const (
	testBase  = "https://cairn.example"
	testToken = domain.OAuthAccessTokenPrefix + "valid"
)

type tokenRepo struct{ port.OAuthServerRepo }

func (tokenRepo) FindAccessToken(_ context.Context, h []byte) (domain.OAuthAccessToken, error) {
	if string(h) != string(oauthserver.HashToken(testToken)) {
		return domain.OAuthAccessToken{}, domain.ErrNotFound
	}
	return domain.OAuthAccessToken{Scope: domain.ScopeActivitiesRead, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func newTestMux() *http.ServeMux {
	mux := http.NewServeMux()
	(&Server{Tokens: &oauthserver.Server{Repo: tokenRepo{}}, BaseURL: testBase}).Mount(mux)
	return mux
}

func TestGetMCP(t *testing.T) {
	mux := newTestMux()
	for _, tc := range []struct {
		name, auth string
		status     int
		challenge  string
	}{
		{"anonymous", "", http.StatusUnauthorized,
			`Bearer resource_metadata="` + testBase + `/.well-known/oauth-protected-resource/mcp"`},
		{"invalid token", "Bearer " + domain.OAuthAccessTokenPrefix + "nope", http.StatusUnauthorized,
			`Bearer resource_metadata="` + testBase + `/.well-known/oauth-protected-resource/mcp", error="invalid_token"`},
		// Streamable HTTP: no SSE stream must be 405, or clients treat the token as rejected.
		{"valid token", "Bearer " + testToken, http.StatusMethodNotAllowed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
			req.Header.Set("Accept", "text/event-stream")
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != tc.challenge {
				t.Errorf("WWW-Authenticate = %q, want %q", got, tc.challenge)
			}
			if tc.status == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != http.MethodPost {
				t.Errorf("Allow = %q, want POST", rec.Header().Get("Allow"))
			}
		})
	}
}

func TestInitializeWithToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	newTestMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestResourceMetadata(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var md struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &md); err != nil {
		t.Fatal(err)
	}
	if md.Resource != testBase+"/mcp" {
		t.Errorf("resource = %q, want the endpoint URL", md.Resource)
	}
	if !slices.Equal(md.AuthorizationServers, []string{testBase}) {
		t.Errorf("authorization_servers = %v", md.AuthorizationServers)
	}
	if want := []string{domain.ScopeActivitiesRead, domain.ScopeProfileRead}; !slices.Equal(md.ScopesSupported, want) {
		t.Errorf("scopes_supported = %v, want %v", md.ScopesSupported, want)
	}
}
