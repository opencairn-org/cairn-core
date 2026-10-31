package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

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

// fakeOIDC records the flow inputs and returns a canned id_token.
type fakeOIDC struct {
	authReq      port.OIDCAuthRequest
	codeVerifier string
	idToken      domain.OIDCIDToken
	err          error
}

var _ port.OIDCClient = (*fakeOIDC)(nil)

func (f *fakeOIDC) AuthCodeURL(_ context.Context, _ domain.OIDCProvider, req port.OIDCAuthRequest) (string, error) {
	f.authReq = req
	return "https://idp.example/authorize?state=" + req.State, f.err
}

func (f *fakeOIDC) Exchange(_ context.Context, _ domain.OIDCProvider, _, _, codeVerifier string) (domain.OIDCIDToken, error) {
	f.codeVerifier = codeVerifier
	return f.idToken, f.err
}

func newOIDCLoginForTest(providers []domain.OIDCProvider, users oidcUserStore, fo *fakeOIDC) *OIDCLogin {
	uc := NewOIDCLogin(providers, nil, nil, users, time.Hour, "http://app.example/", fo)
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
	uc := newOIDCLoginForTest(nil, users, &fakeOIDC{})

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
	uc := newOIDCLoginForTest(nil, users, &fakeOIDC{})

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
	uc := newOIDCLoginForTest(nil, users, &fakeOIDC{})

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
	uc := newOIDCLoginForTest(nil, newFakeUsers(), &fakeOIDC{})
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

	uc := newOIDCLoginForTest(nil, users, &fakeOIDC{})
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

	uc := newOIDCLoginForTest(nil, users, &fakeOIDC{})
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

func TestStartLogin_StateNoncePKCE(t *testing.T) {
	p := testProvider()
	p.UsePKCE = true
	fo := &fakeOIDC{}
	uc := newOIDCLoginForTest([]domain.OIDCProvider{p}, newFakeUsers(), fo)

	out, err := uc.StartLogin(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if out.State == "" || out.Nonce == "" || out.CodeVerifier == "" {
		t.Fatal("StartLogin must produce state, nonce and a PKCE verifier")
	}
	want := port.OIDCAuthRequest{
		RedirectURL: "http://app.example/auth/oidc/callback", State: out.State, Nonce: out.Nonce, CodeChallenge: s256(out.CodeVerifier),
	}
	if fo.authReq != want {
		t.Errorf("auth request = %+v, want %+v", fo.authReq, want)
	}
	if out.AuthURL != "https://idp.example/authorize?state="+out.State {
		t.Errorf("AuthURL = %s", out.AuthURL)
	}
}

func TestStartLogin_NoPKCE(t *testing.T) {
	fo := &fakeOIDC{}
	uc := newOIDCLoginForTest([]domain.OIDCProvider{testProvider()}, newFakeUsers(), fo)
	out, err := uc.StartLogin(context.Background(), "acme")
	if err != nil || out.CodeVerifier != "" || fo.authReq.CodeChallenge != "" {
		t.Fatalf("no-PKCE provider must not send a challenge: %+v %+v %v", out, fo.authReq, err)
	}
}

func TestStartLogin_UnknownProvider(t *testing.T) {
	uc := newOIDCLoginForTest([]domain.OIDCProvider{testProvider()}, newFakeUsers(), &fakeOIDC{})
	if _, err := uc.StartLogin(context.Background(), "nope"); !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("want ErrProviderNotFound, got %v", err)
	}
}

// --- CompleteLogin ----------------------------------------------------------

func TestCompleteLogin_NonceMismatchRejected(t *testing.T) {
	fo := &fakeOIDC{idToken: domain.OIDCIDToken{Subject: "sub", Nonce: "REAL"}}
	uc := newOIDCLoginForTest([]domain.OIDCProvider{testProvider()}, newFakeUsers(), fo)

	_, err := uc.CompleteLogin(context.Background(), CompleteLoginInput{
		ProviderID: "acme", Code: "c", ExpectedNonce: "WRONG",
	})
	if !errors.Is(err, ErrIDTokenVerifyFailed) {
		t.Errorf("nonce mismatch must fail with ErrIDTokenVerifyFailed, got %v", err)
	}
}

func TestCompleteLogin_EmptyExpectedNonceRejected(t *testing.T) {
	fo := &fakeOIDC{idToken: domain.OIDCIDToken{Subject: "sub", Nonce: ""}}
	uc := newOIDCLoginForTest([]domain.OIDCProvider{testProvider()}, newFakeUsers(), fo)

	// An empty expected nonce must be a hard failure, never a skipped check.
	_, err := uc.CompleteLogin(context.Background(), CompleteLoginInput{
		ProviderID: "acme", Code: "c", ExpectedNonce: "",
	})
	if !errors.Is(err, ErrIDTokenVerifyFailed) {
		t.Errorf("empty nonce must fail with ErrIDTokenVerifyFailed, got %v", err)
	}
}

func TestCompleteLogin_ExchangeErrorPassesThrough(t *testing.T) {
	fo := &fakeOIDC{err: fmt.Errorf("%w: boom", port.ErrOIDCCodeExchange)}
	uc := newOIDCLoginForTest([]domain.OIDCProvider{testProvider()}, newFakeUsers(), fo)
	_, err := uc.CompleteLogin(context.Background(), CompleteLoginInput{ProviderID: "acme", Code: "c", ExpectedNonce: "n"})
	if !errors.Is(err, ErrCodeExchangeFailed) {
		t.Errorf("want ErrCodeExchangeFailed, got %v", err)
	}
}

// The PKCE verifier reaches the token endpoint only for PKCE providers.
func TestCompleteLogin_CodeVerifierOnlyWithPKCE(t *testing.T) {
	for _, pkce := range []bool{true, false} {
		p := testProvider()
		p.UsePKCE = pkce
		fo := &fakeOIDC{idToken: domain.OIDCIDToken{Subject: "sub", Nonce: "n"}, err: errors.New("stop")}
		uc := newOIDCLoginForTest([]domain.OIDCProvider{p}, newFakeUsers(), fo)
		_, _ = uc.CompleteLogin(context.Background(), CompleteLoginInput{ProviderID: "acme", Code: "c", CodeVerifier: "v", ExpectedNonce: "n"})
		if got := fo.codeVerifier != ""; got != pkce {
			t.Errorf("pkce=%v: code verifier sent = %v", pkce, got)
		}
	}
}
