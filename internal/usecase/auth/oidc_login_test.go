package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	oidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// --- fakes -----------------------------------------------------------------

// fakeUsers implements only the two UserRepo methods the OIDC provision path
// touches; the embedded nil interface satisfies the rest (never called).
type fakeUsers struct {
	port.UserReader
	port.UserLifecycleStore
	byEmail       map[string]domain.User
	takenUsername map[string]bool
	takenEmail    map[string]bool
	created       []domain.User
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{
		byEmail:       map[string]domain.User{},
		takenUsername: map[string]bool{},
		takenEmail:    map[string]bool{},
	}
}

func (f *fakeUsers) GetUserByEmail(_ context.Context, email string) (domain.User, error) {
	if u, ok := f.byEmail[email]; ok {
		return u, nil
	}
	return domain.User{}, domain.ErrNotFound
}

func (f *fakeUsers) CreateUser(_ context.Context, u domain.User, _ domain.Credentials) error {
	// Wrap the real sentinels so provisionUser's errors.Is checks fire, exactly
	// as the postgres adapter does via mapUserUniqueViolation.
	if f.takenUsername[u.Username] {
		return fmt.Errorf("create user: %w", port.ErrUsernameTaken)
	}
	if f.takenEmail[u.Email] {
		return fmt.Errorf("create user: %w", port.ErrEmailTaken)
	}
	f.takenUsername[u.Username] = true
	f.takenEmail[u.Email] = true
	f.created = append(f.created, u)
	return nil
}

type fakeVerifier struct {
	endpoint oauth2.Endpoint
	idToken  *oidc.IDToken
	verErr   error
}

func (v *fakeVerifier) Endpoint() oauth2.Endpoint { return v.endpoint }
func (v *fakeVerifier) Verifier(*oidc.Config) IDTokenVerifier {
	return &fakeIDV{tok: v.idToken, err: v.verErr}
}

type fakeIDV struct {
	tok *oidc.IDToken
	err error
}

func (v *fakeIDV) Verify(context.Context, string) (*oidc.IDToken, error) { return v.tok, v.err }

func newOIDCLoginForTest(providers []domain.OIDCProvider, users oidcUserStore, fv *fakeVerifier) *OIDCLogin {
	uc := NewOIDCLogin(providers, nil, nil, users, time.Hour, "http://app.example")
	uc.SetVerifierFactory(func(context.Context, string) (OIDCVerifier, error) { return fv, nil })
	uc.SetUUID(func() uuid.UUID { return uuid.MustParse("00000000-0000-0000-0000-000000000001") })
	uc.SetClock(func() time.Time { return time.Unix(1700000000, 0).UTC() })
	return uc
}

func testProvider() domain.OIDCProvider {
	return domain.OIDCProvider{
		ID: "acme", Name: "Acme", IssuerURL: "https://idp.example", ClientID: "cid",
		ClientSecret: "secret", Scopes: []string{"openid", "email", "profile"},
		AutoProvision: true, AutoProvisionRole: domain.UserRoleUser,
	}
}

// --- provisionUser ---------------------------------------------------------

func TestProvisionUser_UsernameCollisionRetries(t *testing.T) {
	users := newFakeUsers()
	users.takenUsername["alice"] = true // base collides; suffix should win
	uc := newOIDCLoginForTest(nil, users, &fakeVerifier{})

	claims := map[string]any{domain.ClaimEmail: "alice@x.com", domain.ClaimUsername: "alice"}
	u, err := uc.provisionUser(context.Background(), testProvider(), claims)
	if err != nil {
		t.Fatalf("provisionUser: %v", err)
	}
	if u.Username != "alice2" {
		t.Errorf("username = %q, want alice2 (suffix after collision)", u.Username)
	}
}

func TestProvisionUser_EmailCollisionFailsCleanly(t *testing.T) {
	users := newFakeUsers()
	users.takenEmail["bob@x.com"] = true
	uc := newOIDCLoginForTest(nil, users, &fakeVerifier{})

	_, err := uc.provisionUser(context.Background(), testProvider(),
		map[string]any{domain.ClaimEmail: "bob@x.com", domain.ClaimUsername: "bob"})
	if !errors.Is(err, port.ErrEmailTaken) {
		t.Errorf("want ErrEmailTaken (no silent takeover), got %v", err)
	}
}

func TestProvisionUser_HonorsAdminRole(t *testing.T) {
	users := newFakeUsers()
	p := testProvider()
	p.AutoProvisionRole = domain.UserRoleAdmin
	uc := newOIDCLoginForTest(nil, users, &fakeVerifier{})

	u, err := uc.provisionUser(context.Background(), p,
		map[string]any{domain.ClaimEmail: "admin@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != domain.UserRoleAdmin {
		t.Errorf("role = %v, want admin (AUTO_PROVISION_ROLE honored)", u.Role)
	}
}

func TestProvisionUser_EmailMissing(t *testing.T) {
	uc := newOIDCLoginForTest(nil, newFakeUsers(), &fakeVerifier{})
	_, err := uc.provisionUser(context.Background(), testProvider(), map[string]any{})
	if !errors.Is(err, ErrEmailMissing) {
		t.Errorf("want ErrEmailMissing, got %v", err)
	}
}

// --- resolveUserForNewIdentity (auto-link / anti-hijack) -------------------

func TestResolveUser_VerifiedEmailLinksToExisting(t *testing.T) {
	users := newFakeUsers()
	existing := domain.User{ID: domain.UserID(uuid.New()), Username: "carol", Email: "carol@x.com"}
	users.byEmail["carol@x.com"] = existing

	uc := newOIDCLoginForTest(nil, users, &fakeVerifier{})
	got, isNew, err := uc.resolveUserForNewIdentity(context.Background(), testProvider(),
		map[string]any{domain.ClaimEmail: "carol@x.com", domain.ClaimEmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	if isNew || got.ID != existing.ID {
		t.Errorf("verified email should link to existing user (isNew=%v)", isNew)
	}
	if len(users.created) != 0 {
		t.Error("must not create a new user when linking")
	}
}

func TestResolveUser_UnverifiedEmailNeverLinks(t *testing.T) {
	users := newFakeUsers()
	users.byEmail["dave@x.com"] = domain.User{ID: domain.UserID(uuid.New()), Email: "dave@x.com"}
	users.takenEmail["dave@x.com"] = true // the existing account owns the address

	uc := newOIDCLoginForTest(nil, users, &fakeVerifier{})
	// Unverified email matching an existing account must NOT link and must NOT
	// take it over — provision is attempted and fails on the email-unique guard.
	_, _, err := uc.resolveUserForNewIdentity(context.Background(), testProvider(),
		map[string]any{domain.ClaimEmail: "dave@x.com", domain.ClaimEmailVerified: false})
	if err == nil {
		t.Fatal("unverified email matching an existing account must not succeed")
	}
	if !errors.Is(err, port.ErrEmailTaken) {
		t.Errorf("want ErrEmailTaken (anti-hijack), got %v", err)
	}
}

// --- StartLogin ------------------------------------------------------------

func TestStartLogin_BuildsAuthURLWithStateNoncePKCE(t *testing.T) {
	p := testProvider()
	p.UsePKCE = true
	fv := &fakeVerifier{endpoint: oauth2.Endpoint{
		AuthURL: "https://idp.example/authorize", TokenURL: "https://idp.example/token",
	}}
	uc := newOIDCLoginForTest([]domain.OIDCProvider{p}, newFakeUsers(), fv)

	out, err := uc.StartLogin(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if out.State == "" || out.Nonce == "" || out.CodeVerifier == "" {
		t.Fatal("StartLogin must produce state, nonce and a PKCE verifier")
	}
	for _, want := range []string{
		"https://idp.example/authorize",
		"client_id=cid",
		"state=" + out.State,
		"nonce=" + out.Nonce,
		"code_challenge=",
		"code_challenge_method=S256",
		"redirect_uri=",
	} {
		if !strings.Contains(out.AuthURL, want) {
			t.Errorf("AuthURL missing %q\n  got %s", want, out.AuthURL)
		}
	}
}

func TestStartLogin_UnknownProvider(t *testing.T) {
	uc := newOIDCLoginForTest([]domain.OIDCProvider{testProvider()}, newFakeUsers(), &fakeVerifier{})
	if _, err := uc.StartLogin(context.Background(), "nope"); !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("want ErrProviderNotFound, got %v", err)
	}
}

// --- CompleteLogin: mandatory nonce ----------------------------------------

func tokenEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a","id_token":"tok","token_type":"Bearer","expires_in":3600}`))
	}))
}

func TestCompleteLogin_NonceMismatchRejected(t *testing.T) {
	srv := tokenEndpoint(t)
	defer srv.Close()
	p := testProvider()
	fv := &fakeVerifier{
		endpoint: oauth2.Endpoint{AuthURL: "https://idp.example/authorize", TokenURL: srv.URL},
		idToken:  &oidc.IDToken{Subject: "sub", Nonce: "REAL"},
	}
	uc := newOIDCLoginForTest([]domain.OIDCProvider{p}, newFakeUsers(), fv)

	_, err := uc.CompleteLogin(context.Background(), CompleteLoginInput{
		ProviderID: "acme", Code: "c", ExpectedNonce: "WRONG",
	})
	if !errors.Is(err, ErrIDTokenVerifyFailed) {
		t.Errorf("nonce mismatch must fail with ErrIDTokenVerifyFailed, got %v", err)
	}
}

func TestCompleteLogin_EmptyExpectedNonceRejected(t *testing.T) {
	srv := tokenEndpoint(t)
	defer srv.Close()
	fv := &fakeVerifier{
		endpoint: oauth2.Endpoint{AuthURL: "https://idp.example/authorize", TokenURL: srv.URL},
		idToken:  &oidc.IDToken{Subject: "sub", Nonce: "REAL"},
	}
	uc := newOIDCLoginForTest([]domain.OIDCProvider{testProvider()}, newFakeUsers(), fv)

	// An empty expected nonce must be a hard failure, never a skipped check.
	_, err := uc.CompleteLogin(context.Background(), CompleteLoginInput{
		ProviderID: "acme", Code: "c", ExpectedNonce: "",
	})
	if !errors.Is(err, ErrIDTokenVerifyFailed) {
		t.Errorf("empty nonce must fail with ErrIDTokenVerifyFailed, got %v", err)
	}
}
