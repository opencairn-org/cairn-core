package oidc

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// idp serves discovery and a token endpoint; signature checks are covered by
// the usecase/auth integration round trip.
type idp struct {
	*httptest.Server
	tokenStatus  int
	tokenBody    string
	codeVerifier string
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	m := &idp{tokenStatus: http.StatusOK, tokenBody: `{"access_token":"a","token_type":"Bearer"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": m.URL, "authorization_endpoint": m.URL + "/authorize",
			"token_endpoint": m.URL + "/token", "jwks_uri": m.URL + "/keys",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		m.codeVerifier = r.FormValue("code_verifier")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(m.tokenStatus)
		_, _ = w.Write([]byte(m.tokenBody))
	})
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Close)
	return m
}

func provider(issuer string) domain.OIDCProvider {
	return domain.OIDCProvider{ID: "acme", IssuerURL: issuer, ClientID: "cid", ClientSecret: "s", Scopes: []string{"email"}}
}

func TestAuthCodeURL(t *testing.T) {
	m := newIDP(t)
	u, err := New().AuthCodeURL(t.Context(), provider(m.URL), port.OIDCAuthRequest{
		RedirectURL: "http://app/cb", State: "st", Nonce: "no", CodeChallenge: "ch",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		m.URL + "/authorize", "client_id=cid", "state=st", "nonce=no", "code_challenge=ch",
		"code_challenge_method=S256", "redirect_uri=http%3A%2F%2Fapp%2Fcb", "scope=openid+email",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("AuthURL missing %q\n  got %s", want, u)
		}
	}
	u, _ = New().AuthCodeURL(t.Context(), provider(m.URL), port.OIDCAuthRequest{State: "st", Nonce: "no"})
	if strings.Contains(u, "code_challenge") {
		t.Errorf("no challenge → no PKCE params: %s", u)
	}
}

func TestExchangeErrors(t *testing.T) {
	m := newIDP(t)
	if _, err := New().Exchange(t.Context(), provider(m.URL), "http://app/cb", "c", "ver"); !errors.Is(err, port.ErrOIDCIDTokenInvalid) {
		t.Errorf("missing id_token: %v", err)
	}
	if m.codeVerifier != "ver" {
		t.Errorf("code_verifier = %q", m.codeVerifier)
	}
	m.tokenStatus, m.tokenBody = http.StatusBadRequest, `{"error":"invalid_grant"}`
	if _, err := New().Exchange(t.Context(), provider(m.URL), "http://app/cb", "c", ""); !errors.Is(err, port.ErrOIDCCodeExchange) {
		t.Errorf("token endpoint 400: %v", err)
	}
	if m.codeVerifier != "" {
		t.Errorf("no verifier expected, got %q", m.codeVerifier)
	}
	if _, err := New().Exchange(t.Context(), provider("http://127.0.0.1:1"), "", "c", ""); err == nil ||
		errors.Is(err, port.ErrOIDCCodeExchange) || errors.Is(err, port.ErrOIDCIDTokenInvalid) {
		t.Errorf("discovery failure must be unclassified: %v", err)
	}
}
