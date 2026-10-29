package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	jwtv5 "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// mockIDP is a minimal but REAL OIDC provider over httptest: it serves
// discovery + a JWKS and signs id_tokens, so the production go-oidc verifier
// (discovery → JWKS fetch → RS256 signature → iss/aud/exp) runs for real. The
// token endpoint echoes the posted `code` back as the id_token nonce, which is
// how the test threads StartLogin's nonce through to CompleteLogin.
type mockIDP struct {
	*httptest.Server
	priv   *rsa.PrivateKey
	claims map[string]any // claims to embed (besides iss/aud/exp/iat/nonce)
}

func newMockIDP(t *testing.T, extraClaims map[string]any) *mockIDP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockIDP{priv: priv, claims: extraClaims}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(w, map[string]any{
			"issuer":                                m.URL,
			"authorization_endpoint":                m.URL + "/authorize",
			"token_endpoint":                        m.URL + "/token",
			"jwks_uri":                              m.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &priv.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"},
		}}
		writeJSONResp(w, set)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		now := time.Now()
		claims := jwtv5.MapClaims{
			"iss":   m.URL,
			"aud":   "cid",
			"sub":   "sub-abc",
			"exp":   now.Add(time.Hour).Unix(),
			"iat":   now.Unix(),
			"nonce": r.FormValue("code"), // echo the code as the nonce
		}
		for k, v := range m.claims {
			claims[k] = v
		}
		tok := jwtv5.NewWithClaims(jwtv5.SigningMethodRS256, claims)
		tok.Header["kid"] = "test"
		idToken, err := tok.SignedString(priv)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSONResp(w, map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
		})
	})
	m.Server = httptest.NewServer(mux)
	return m
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- identity + session fakes (in-memory) ----------------------------------

type fakeIdentities struct {
	bySubject map[string]domain.LinkedIdentity
	created   []domain.LinkedIdentity
}

func newFakeIdentities() *fakeIdentities {
	return &fakeIdentities{bySubject: map[string]domain.LinkedIdentity{}}
}
func (f *fakeIdentities) FindBySubject(_ context.Context, provider, subject string) (domain.LinkedIdentity, error) {
	if li, ok := f.bySubject[provider+"|"+subject]; ok {
		return li, nil
	}
	return domain.LinkedIdentity{}, domain.ErrNotFound
}
func (f *fakeIdentities) Create(_ context.Context, li domain.LinkedIdentity) (domain.LinkedIdentityID, error) {
	li.ID = domain.LinkedIdentityID(uuid.New())
	f.bySubject[li.Provider+"|"+li.Subject] = li
	f.created = append(f.created, li)
	return li.ID, nil
}
func (f *fakeIdentities) ListForUser(context.Context, domain.UserID) ([]domain.LinkedIdentity, error) {
	return f.created, nil
}

func (f *fakeIdentities) Delete(context.Context, domain.UserID, domain.LinkedIdentityID) error {
	return nil
}

func (f *fakeIdentities) TouchLastUsed(context.Context, domain.LinkedIdentityID, map[string]any) error {
	return nil
}

type fakeSessions struct {
	port.SessionRepo
	created []domain.Session
}

func (f *fakeSessions) Create(_ context.Context, s domain.Session) (domain.SessionID, error) {
	f.created = append(f.created, s)
	return domain.SessionID(uuid.New()), nil
}

// --- the integration test --------------------------------------------------

func TestOIDCLogin_FullRoundTripProvisionsAndLinks(t *testing.T) {
	idp := newMockIDP(t, map[string]any{
		"email":              "newuser@idp.test",
		"email_verified":     true,
		"preferred_username": "newuser",
		"name":               "New User",
	})
	defer idp.Close()

	p := domain.OIDCProvider{
		ID: "mock", Name: "Mock", IssuerURL: idp.URL, ClientID: "cid", ClientSecret: "sec",
		Scopes: []string{"openid", "email", "profile"}, UsePKCE: true,
		AutoProvision: true, AutoProvisionRole: domain.UserRoleUser,
	}
	users := newFakeUsers()
	ids := newFakeIdentities()
	sess := &fakeSessions{}
	uc := NewOIDCLogin([]domain.OIDCProvider{p}, ids, sess, users, time.Hour, "http://app.example")

	ctx := context.Background()

	// 1) StartLogin runs REAL discovery against the mock IdP.
	out, err := uc.StartLogin(ctx, "mock")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	// 2) CompleteLogin: code=nonce so the IdP's id_token echoes the right nonce.
	//    This drives the real verifier (JWKS fetch + RS256 verify + iss/aud/exp).
	res, err := uc.CompleteLogin(ctx, CompleteLoginInput{
		ProviderID: "mock", Code: out.Nonce, CodeVerifier: out.CodeVerifier, ExpectedNonce: out.Nonce,
	})
	if err != nil {
		t.Fatalf("CompleteLogin (first): %v", err)
	}
	if !res.NewUser {
		t.Error("first login should provision a new user")
	}
	if len(users.created) != 1 || users.created[0].Email != "newuser@idp.test" || users.created[0].Username != "newuser" {
		t.Errorf("expected one provisioned user newuser@idp.test, got %+v", users.created)
	}
	if len(ids.created) != 1 || ids.created[0].Provider != "mock" || ids.created[0].Subject != "sub-abc" {
		t.Errorf("expected one linked identity (mock, sub-abc), got %+v", ids.created)
	}
	if len(sess.created) != 1 || sess.created[0].AuthMethod != domain.SessionAuthOIDC || res.BearerToken == "" {
		t.Errorf("expected one OIDC session + a bearer token, got %+v", sess.created)
	}

	// 3) A second login for the SAME subject links to the existing user — no
	//    duplicate provisioning.
	out2, err := uc.StartLogin(ctx, "mock")
	if err != nil {
		t.Fatal(err)
	}
	res2, err := uc.CompleteLogin(ctx, CompleteLoginInput{
		ProviderID: "mock", Code: out2.Nonce, CodeVerifier: out2.CodeVerifier, ExpectedNonce: out2.Nonce,
	})
	if err != nil {
		t.Fatalf("CompleteLogin (second): %v", err)
	}
	if res2.NewUser {
		t.Error("second login for the same subject must NOT create a new user")
	}
	if len(users.created) != 1 {
		t.Errorf("still expected exactly one user after re-login, got %d", len(users.created))
	}
}
