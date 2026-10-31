package oauthserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// fakeRepo keeps clients, codes and tokens in memory.
type fakeRepo struct {
	port.OAuthServerRepo
	clients  map[string]domain.OAuthClient
	codes    map[string]domain.OAuthAuthorizationCode
	access   map[string]domain.OAuthAccessToken
	refresh  map[string]domain.OAuthRefreshToken
	consumed map[string]bool
}

func newFakeRepo(clients ...domain.OAuthClient) *fakeRepo {
	f := &fakeRepo{
		clients: map[string]domain.OAuthClient{}, codes: map[string]domain.OAuthAuthorizationCode{},
		access: map[string]domain.OAuthAccessToken{}, refresh: map[string]domain.OAuthRefreshToken{},
		consumed: map[string]bool{},
	}
	for _, c := range clients {
		f.clients[c.ClientID] = c
	}
	return f
}

func (f *fakeRepo) CreateClient(_ context.Context, c domain.OAuthClient) error {
	f.clients[c.ClientID] = c
	return nil
}
func (f *fakeRepo) GetClient(_ context.Context, id string) (domain.OAuthClient, error) {
	c, ok := f.clients[id]
	if !ok {
		return c, domain.ErrNotFound
	}
	return c, nil
}
func (f *fakeRepo) CreateAuthCode(_ context.Context, c domain.OAuthAuthorizationCode, h []byte) error {
	f.codes[string(h)] = c
	return nil
}
func (f *fakeRepo) ConsumeAuthCode(_ context.Context, h []byte, now time.Time) (domain.OAuthAuthorizationCode, error) {
	c, ok := f.codes[string(h)]
	if !ok || f.consumed[string(h)] || !now.Before(c.ExpiresAt) {
		return c, domain.ErrNotFound
	}
	f.consumed[string(h)] = true
	return c, nil
}
func (f *fakeRepo) CreateAccessToken(_ context.Context, t domain.OAuthAccessToken, h []byte) error {
	f.access[string(h)] = t
	return nil
}
func (f *fakeRepo) FindAccessToken(_ context.Context, h []byte) (domain.OAuthAccessToken, error) {
	t, ok := f.access[string(h)]
	if !ok {
		return t, domain.ErrNotFound
	}
	return t, nil
}
func (f *fakeRepo) RevokeAccessToken(_ context.Context, h []byte) error {
	delete(f.access, string(h))
	return nil
}
func (f *fakeRepo) CreateRefreshToken(_ context.Context, t domain.OAuthRefreshToken, h []byte) error {
	f.refresh[string(h)] = t
	return nil
}
func (f *fakeRepo) FindRefreshToken(_ context.Context, h []byte) (domain.OAuthRefreshToken, error) {
	t, ok := f.refresh[string(h)]
	if !ok {
		return t, domain.ErrNotFound
	}
	return t, nil
}
func (f *fakeRepo) RevokeRefreshToken(_ context.Context, h []byte) error {
	delete(f.refresh, string(h))
	return nil
}

var app = domain.OAuthClient{
	ClientID: "cairn_dcr_app", Name: "App", RedirectURIs: []string{"https://app.example/cb"},
	GrantTypes: []string{"authorization_code", "refresh_token"}, Scopes: domain.ReadOnlyScopes,
}

func newServer(repo *fakeRepo) *Server {
	return &Server{Repo: repo, Config: Config{AuthCodeTTL: time.Minute, AccessTokenTTL: time.Hour, RefreshTokenTTL: 24 * time.Hour}}
}

func protoCode(err error) string {
	var oe *Error
	if errors.As(err, &oe) {
		return oe.Code
	}
	return ""
}

func TestAuthorizeValidates(t *testing.T) {
	s := newServer(newFakeRepo(app))
	ctx := context.Background()
	ok := AuthorizeRequest{ClientID: app.ClientID, RedirectURI: "https://app.example/cb", ResponseType: "code", CodeChallenge: "x"}
	if uri, err := s.Authorize(ctx, ok); err != nil || uri != "https://app.example/cb" {
		t.Fatalf("valid request: %q %v", uri, err)
	}
	bad := ok
	bad.ClientID = "nope"
	if _, err := s.Authorize(ctx, bad); !errors.Is(err, ErrUnknownClient) {
		t.Fatalf("unknown client: %v", err)
	}
	bad = ok
	bad.RedirectURI = "https://evil.example/cb"
	if _, err := s.Authorize(ctx, bad); !errors.Is(err, ErrInvalidRedirect) {
		t.Fatalf("foreign redirect: %v", err)
	}
	bad = ok
	bad.CodeChallenge = ""
	if uri, err := s.Authorize(ctx, bad); protoCode(err) != "invalid_request" || uri == "" {
		t.Fatalf("no PKCE: %q %v", uri, err)
	}
	bad = ok
	bad.CodeChallengeMethod = "plain"
	if _, err := s.Authorize(ctx, bad); protoCode(err) != "invalid_request" {
		t.Fatalf("plain PKCE: %v", err)
	}
}

func TestCodeFlowAndRefreshRotation(t *testing.T) {
	repo := newFakeRepo(app)
	s := newServer(repo)
	ctx := context.Background()
	user := domain.UserID(uuid.New())
	verifier := "a-long-enough-pkce-verifier-for-the-test-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	redirect, err := s.Decide(ctx, user, Decision{
		ClientID: app.ClientID, RedirectURI: "https://app.example/cb", Scope: "activities:read profile:write",
		State: "st", CodeChallenge: challenge, Approve: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(redirect)
	code := u.Query().Get("code")
	if u.Query().Get("state") != "st" || !strings.HasPrefix(code, domain.OAuthAuthCodePrefix) {
		t.Fatalf("redirect %q", redirect)
	}
	creds := ClientCredentials{ID: app.ClientID}
	if _, err := s.ExchangeCode(ctx, creds, code, "https://app.example/cb", "wrong"); protoCode(err) != "invalid_grant" {
		t.Fatalf("bad verifier: %v", err)
	}
	// The failed attempt consumed the code: codes are one-time.
	redirect, _ = s.Decide(ctx, user, Decision{ClientID: app.ClientID, RedirectURI: "https://app.example/cb", Scope: "activities:read", CodeChallenge: challenge, Approve: true})
	u, _ = url.Parse(redirect)
	grant, err := s.ExchangeCode(ctx, creds, u.Query().Get("code"), "https://app.example/cb", verifier)
	if err != nil || grant.Scope != "activities:read" || grant.ExpiresIn != time.Hour {
		t.Fatalf("exchange: %+v %v", grant, err)
	}
	if at, ok := s.AccessToken(ctx, grant.AccessToken); !ok || at.UserID != user {
		t.Fatalf("access token not resolvable")
	}

	next, err := s.Refresh(ctx, creds, grant.RefreshToken, "")
	if err != nil || next.RefreshToken == grant.RefreshToken {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := s.Refresh(ctx, creds, grant.RefreshToken, ""); protoCode(err) != "invalid_grant" {
		t.Fatalf("rotated token reused: %v", err)
	}
	if _, err := s.Refresh(ctx, ClientCredentials{ID: "nope"}, next.RefreshToken, ""); protoCode(err) != "invalid_client" {
		t.Fatalf("unknown client: %v", err)
	}

	s.Revoke(ctx, next.AccessToken)
	if _, ok := s.AccessToken(ctx, next.AccessToken); ok {
		t.Fatal("revoked token still resolves")
	}
}

func TestDecideDenied(t *testing.T) {
	s := newServer(newFakeRepo(app))
	redirect, err := s.Decide(context.Background(), domain.UserID(uuid.New()), Decision{ClientID: app.ClientID, RedirectURI: "https://app.example/cb", State: "s"})
	if err != nil || redirect != "https://app.example/cb?error=access_denied&state=s" {
		t.Fatalf("deny: %q %v", redirect, err)
	}
	if _, err := s.Decide(context.Background(), domain.UserID(uuid.New()), Decision{ClientID: app.ClientID, RedirectURI: "https://app.example/cb", Approve: true}); !errors.Is(err, ErrMissingChallenge) {
		t.Fatalf("approve without PKCE: %v", err)
	}
}

func TestRegister(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(repo)
	ctx := context.Background()
	if _, err := s.Register(ctx, Registration{}); protoCode(err) != "invalid_redirect_uri" {
		t.Fatalf("no redirect: %v", err)
	}
	pub, err := s.Register(ctx, Registration{RedirectURIs: []string{"https://a.example/cb"}, ClientName: strings.Repeat("n", 300)})
	if err != nil || pub.Secret != "" || !pub.Client.IsPublic() || len([]rune(pub.Client.Name)) != registrationFieldMax {
		t.Fatalf("public client: %+v %v", pub, err)
	}
	if strings.Join(pub.Client.Scopes, " ") != strings.Join(domain.ReadOnlyScopes, " ") {
		t.Fatalf("default scopes %v", pub.Client.Scopes)
	}
	conf, err := s.Register(ctx, Registration{RedirectURIs: []string{"https://a.example/cb"}, TokenEndpointAuthMethod: "client_secret_basic"})
	if err != nil || conf.Secret == "" || !repo.clients[conf.Client.ClientID].VerifySecret(conf.Secret) {
		t.Fatalf("confidential client: %v", err)
	}
	if _, err := s.Refresh(ctx, ClientCredentials{ID: conf.Client.ClientID, Secret: "wrong"}, domain.OAuthRefreshTokenPrefix+"x", ""); protoCode(err) != "invalid_client" {
		t.Fatalf("wrong secret: %v", err)
	}
}

func TestAppendQuery(t *testing.T) {
	got := AppendQuery("https://app.example/cb?keep=1", map[string]string{"code": "c", "state": "", "error": "e r"})
	if got != "https://app.example/cb?code=c&error=e+r&keep=1" {
		t.Errorf("AppendQuery = %q", got)
	}
}
