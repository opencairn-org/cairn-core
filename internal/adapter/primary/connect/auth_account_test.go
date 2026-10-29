package connect

import (
	"context"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
)

type fakePATRepo struct {
	port.PATRepo
	list    []domain.PersonalAccessToken
	revoked []domain.PATID
}

func (f *fakePATRepo) Create(_ context.Context, p domain.PersonalAccessToken, _ []byte) (domain.PATID, error) {
	p.ID = domain.PATID(uuid.New())
	f.list = append(f.list, p)
	return p.ID, nil
}
func (f *fakePATRepo) ListForUser(context.Context, domain.UserID) ([]domain.PersonalAccessToken, error) {
	return f.list, nil
}
func (f *fakePATRepo) Revoke(_ context.Context, _ domain.UserID, id domain.PATID) error {
	f.revoked = append(f.revoked, id)
	return nil
}

type listingSessions struct {
	fakeSessions
	active []domain.Session
}

func (f *listingSessions) ListForUser(context.Context, domain.UserID) ([]domain.Session, error) {
	return f.active, nil
}

type fakeUserRepo struct {
	port.UserRepo
	user domain.User
}

func (f *fakeUserRepo) GetUser(context.Context, domain.UserID) (domain.User, error) {
	return f.user, nil
}
func (f *fakeUserRepo) UpdateProfile(_ context.Context, _ domain.UserID, p port.ProfileUpdate) error {
	f.user.DisplayName, f.user.Timezone = p.DisplayName, p.Timezone
	return nil
}

func authedCtx(t *testing.T, uid domain.UserID, sid domain.SessionID) context.Context {
	t.Helper()
	return context.WithValue(context.WithValue(t.Context(), ctxKeyUserID, uid), ctxKeySessionID, sid)
}

func TestPersonalAccessTokensRPC(t *testing.T) {
	uid := domain.UserID(uuid.New())
	repo := &fakePATRepo{}
	s := &AuthServer{PATs: &account.Tokens{PATs: repo}}
	ctx := authedCtx(t, uid, domain.SessionID(uuid.New()))

	if _, err := (&AuthServer{}).ListPersonalAccessTokens(ctx, &v1.ListPersonalAccessTokensRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("unwired: %v", err)
	}
	if _, err := s.CreatePersonalAccessToken(t.Context(), &v1.CreatePersonalAccessTokenRequest{Name: "x"}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
	if _, err := s.CreatePersonalAccessToken(ctx, &v1.CreatePersonalAccessTokenRequest{Name: " "}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("blank: %v", err)
	}
	exp := timestamppb.New(time.Now().Add(24 * time.Hour))
	res, err := s.CreatePersonalAccessToken(ctx, &v1.CreatePersonalAccessTokenRequest{Name: "cli", Scopes: []string{"activities:read"}, ExpiresAt: exp})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetSecret() == "" || res.GetToken().GetName() != "cli" || res.GetToken().GetExpiresAt() == nil || res.GetToken().GetScopes()[0] != "activities:read" {
		t.Fatalf("create %v", res)
	}
	now := time.Now()
	repo.list = append(repo.list, domain.PersonalAccessToken{ID: domain.PATID(uuid.New()), Name: "gone", RevokedAt: &now})
	list, err := s.ListPersonalAccessTokens(ctx, &v1.ListPersonalAccessTokensRequest{})
	if err != nil || len(list.GetTokens()) != 1 || list.GetTokens()[0].GetId() != res.GetToken().GetId() {
		t.Fatalf("list hides revoked: %v %v", err, list)
	}
	if _, err := s.RevokePersonalAccessToken(ctx, &v1.RevokePersonalAccessTokenRequest{Id: "nope"}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := s.RevokePersonalAccessToken(ctx, &v1.RevokePersonalAccessTokenRequest{Id: res.GetToken().GetId()}); err != nil || len(repo.revoked) != 1 {
		t.Fatalf("revoke: %v %v", err, repo.revoked)
	}
}

func TestSessionsRPC(t *testing.T) {
	uid := domain.UserID(uuid.New())
	cur, other := domain.SessionID(uuid.New()), domain.SessionID(uuid.New())
	repo := &listingSessions{active: []domain.Session{
		{ID: cur, UserID: uid, AuthMethod: domain.SessionAuthPassword, ExpiresAt: time.Now().Add(time.Hour)},
		{ID: other, UserID: uid, AuthMethod: domain.SessionAuthOIDC, IPGeoSummary: "Berlin, DE", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	s := &AuthServer{SessionMgmt: &account.Sessions{Repo: repo}}
	ctx := authedCtx(t, uid, cur)

	res, err := s.ListSessions(ctx, &v1.ListSessionsRequest{})
	if err != nil || len(res.GetSessions()) != 2 {
		t.Fatalf("list: %v %v", err, res)
	}
	if !res.GetSessions()[0].GetIsCurrent() || res.GetSessions()[1].GetIsCurrent() {
		t.Fatal("is_current marks the cookie session")
	}
	if res.GetSessions()[1].GetAuthMethod() != v1.AuthMethod_AUTH_METHOD_OIDC || res.GetSessions()[1].GetIpGeoSummary() != "Berlin, DE" {
		t.Fatalf("session %v", res.GetSessions()[1])
	}
	if _, err := s.RevokeSession(ctx, &v1.RevokeSessionRequest{Id: uuid.NewString()}); connectrpc.CodeOf(err) != connectrpc.CodeNotFound {
		t.Fatalf("foreign: %v", err)
	}
	if _, err := s.RevokeSession(ctx, &v1.RevokeSessionRequest{Id: uuid.UUID(other).String()}); err != nil || len(repo.revoked) != 1 || repo.revoked[0] != other {
		t.Fatalf("revoke: %v %v", err, repo.revoked)
	}
}

func TestLinkedIdentitiesRPC(t *testing.T) {
	uid := domain.UserID(uuid.New())
	only := domain.LinkedIdentityID(uuid.New())
	repo := &fakeIdentityRepo{links: []domain.LinkedIdentity{{ID: only, UserID: uid, Provider: "google", Subject: "sub-1", Email: "a@x.test", LinkedAt: time.Now()}}}
	users := &fakeUserRepo{user: domain.User{ID: uid, Username: "alice"}}
	ids := &account.Identities{Repo: repo, Users: &credUsers{fakeUserRepo: users}, Providers: []domain.OIDCProvider{{ID: "google", Name: "Google"}}}
	s := &AuthServer{Identities: ids}
	ctx := authedCtx(t, uid, domain.SessionID(uuid.New()))

	res, err := s.ListLinkedIdentities(ctx, &v1.ListLinkedIdentitiesRequest{})
	if err != nil || len(res.GetIdentities()) != 1 {
		t.Fatalf("list: %v %v", err, res)
	}
	li := res.GetIdentities()[0]
	if li.GetOidcClientId() != "google" || li.GetOidcClientDisplayName() != "Google" || li.GetEmail() != "a@x.test" || li.GetSubject() != "sub-1" {
		t.Fatalf("identity %v", li)
	}
	if _, err := s.UnlinkOIDCIdentity(ctx, &v1.UnlinkOIDCIdentityRequest{IdentityId: uuid.UUID(only).String()}); connectrpc.CodeOf(err) != connectrpc.CodeFailedPrecondition {
		t.Fatalf("last credential: %v", err)
	}
}

type fakeIdentityRepo struct {
	port.LinkedIdentityRepo
	links []domain.LinkedIdentity
}

func (f *fakeIdentityRepo) ListForUser(context.Context, domain.UserID) ([]domain.LinkedIdentity, error) {
	return f.links, nil
}

// credUsers adds a password-less credentials read to fakeUserRepo.
type credUsers struct{ *fakeUserRepo }

func (c *credUsers) GetCredentialsByLogin(context.Context, string) (domain.User, domain.Credentials, error) {
	return c.user, domain.Credentials{UserID: c.user.ID}, nil
}

func TestUpdateProfileRPC(t *testing.T) {
	uid := domain.UserID(uuid.New())
	users := &fakeUserRepo{user: domain.User{ID: uid, DisplayName: "Alice", Timezone: "UTC", Locale: "en", Units: domain.UserUnitsMetric}}
	s := &AuthServer{Profiles: &account.Profiles{Users: users}}
	ctx := authedCtx(t, uid, domain.SessionID(uuid.New()))
	str := func(v string) *string { return &v }

	if _, err := s.UpdateProfile(ctx, &v1.UpdateProfileRequest{FieldMask: []string{"email"}, Email: str("b@x.test")}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("email refused: %v", err)
	}
	if _, err := s.UpdateProfile(ctx, &v1.UpdateProfileRequest{FieldMask: []string{"role"}}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("unknown mask: %v", err)
	}
	if _, err := s.UpdateProfile(ctx, &v1.UpdateProfileRequest{Timezone: str("Nowhere/Land")}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("bad timezone: %v", err)
	}
	// Mask limits the applied fields: display_name set but masked out stays.
	res, err := s.UpdateProfile(ctx, &v1.UpdateProfileRequest{FieldMask: []string{"timezone"}, Timezone: str("Europe/Berlin"), DisplayName: str("Bob")})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetUser().GetTimezone() != "Europe/Berlin" || res.GetUser().GetDisplayName() != "Alice" {
		t.Fatalf("user %v", res.GetUser())
	}
}

func TestPasswordRPCs(t *testing.T) {
	s := &AuthServer{}
	// Unwired reset request still answers generically.
	res, err := s.RequestPasswordReset(t.Context(), &v1.RequestPasswordResetRequest{Email: "a@x.test"})
	if err != nil || res.GetEmailDispatchAttempted() {
		t.Fatalf("unwired request: %v %v", err, res)
	}
	if _, err := s.ChangePassword(t.Context(), &v1.ChangePasswordRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous change: %v", err)
	}
	if _, err := s.ConfirmPasswordReset(t.Context(), &v1.ConfirmPasswordResetRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("unwired confirm: %v", err)
	}
}
