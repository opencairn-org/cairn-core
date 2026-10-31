package connect

import (
	"context"
	"errors"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/invite"
	"github.com/opencairn-org/cairn-core/internal/usecase/quota"
)

type fakeSessions struct {
	port.SessionRepo
	revoked    []domain.SessionID
	revokedAll []domain.UserID
}

func (f *fakeSessions) Revoke(_ context.Context, id domain.SessionID) error {
	f.revoked = append(f.revoked, id)
	return nil
}
func (f *fakeSessions) RevokeAllForUser(_ context.Context, id domain.UserID) error {
	f.revokedAll = append(f.revokedAll, id)
	return nil
}

type fakeQuotas struct {
	port.QuotaRepo
	override map[domain.UserID]*int
}

func (f *fakeQuotas) GetMaxActivities(_ context.Context, id domain.UserID) (*int, error) {
	return f.override[id], nil
}

type fakeRedeemer struct {
	in  invite.RedeemInviteInput
	err error
}

func (f *fakeRedeemer) Execute(_ context.Context, in invite.RedeemInviteInput) (domain.User, error) {
	f.in = in
	if f.err != nil {
		return domain.User{}, f.err
	}
	return domain.User{ID: domain.UserID(uuid.New()), Username: in.Username, Email: in.Email, EmailVerified: false, Role: domain.UserRoleUser, Status: domain.UserStatusActive}, nil
}

func TestGetInstanceInfo(t *testing.T) {
	built := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	s := &AuthServer{Instance: InstanceInfo{Version: "v0.6.0", Commit: "abc123", BuildTime: built, FederationEnabled: true, OAuthEnabled: true}}
	res, err := s.GetInstanceInfo(t.Context(), &v1.GetInstanceInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetVersion() != "v0.6.0" || res.GetCommit() != "abc123" || !res.GetFederationEnabled() || !res.GetOauthEnabled() || res.GetRegistrationOpen() {
		t.Fatalf("response %v", res)
	}
	if res.GetBuildTime().AsTime() != built || len(res.GetSupportedScopes()) != len(domain.SupportedScopes) {
		t.Fatalf("build time / scopes wrong: %v", res)
	}
	s.Instance.BuildTime = time.Time{}
	if res, _ := s.GetInstanceInfo(t.Context(), &v1.GetInstanceInfoRequest{}); res.BuildTime != nil {
		t.Fatal("unknown build time must be unset")
	}
}

func TestGetQuota(t *testing.T) {
	repo := inmem.NewActivityRepo()
	user := domain.UserID(uuid.New())
	for range 3 {
		id := domain.ActivityID(uuid.New())
		repo.Activities[id] = domain.Activity{ID: id, UserID: user}
	}
	ctx := context.WithValue(t.Context(), ctxKeyUserID, user)
	s := &AuthServer{Quota: quota.NewPolicy(repo, nil, 3)}

	res, err := s.GetQuota(ctx, &v1.GetQuotaRequest{})
	if err != nil || res.GetActivitiesUsed() != 3 || res.GetActivitiesLimit() != 3 || res.GetUnlimited() || !res.GetOverLimit() {
		t.Fatalf("default limit: %v %v", res, err)
	}
	ten := 10
	s.Quota = quota.NewPolicy(repo, &fakeQuotas{override: map[domain.UserID]*int{user: &ten}}, 3)
	if res, _ := s.GetQuota(ctx, &v1.GetQuotaRequest{}); res.GetActivitiesLimit() != 10 || res.GetOverLimit() {
		t.Fatalf("override: %v", res)
	}
	zero := 0
	s.Quota = quota.NewPolicy(repo, &fakeQuotas{override: map[domain.UserID]*int{user: &zero}}, 3)
	if res, _ := s.GetQuota(ctx, &v1.GetQuotaRequest{}); !res.GetUnlimited() || res.GetOverLimit() {
		t.Fatalf("unlimited: %v", res)
	}
	if _, err := s.GetQuota(t.Context(), &v1.GetQuotaRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
}

func TestLogout(t *testing.T) {
	user := domain.UserID(uuid.New())
	sid := domain.SessionID(uuid.New())
	sessions := &fakeSessions{}
	s := &AuthServer{Sessions: sessions}
	cookieCtx := context.WithValue(context.WithValue(t.Context(), ctxKeyUserID, user), ctxKeySessionID, sid)

	if _, err := s.Logout(cookieCtx, &v1.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(sessions.revoked) != 1 || sessions.revoked[0] != sid || len(sessions.revokedAll) != 0 {
		t.Fatalf("revoked %v all %v", sessions.revoked, sessions.revokedAll)
	}
	if _, err := s.Logout(cookieCtx, &v1.LogoutRequest{AllDevices: true}); err != nil {
		t.Fatal(err)
	}
	if len(sessions.revokedAll) != 1 || sessions.revokedAll[0] != user {
		t.Fatalf("revokedAll %v", sessions.revokedAll)
	}
	// A bearer caller (PAT/OAuth) has no session.
	bearerCtx := context.WithValue(t.Context(), ctxKeyUserID, user)
	if _, err := s.Logout(bearerCtx, &v1.LogoutRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeFailedPrecondition {
		t.Fatalf("bearer: %v", err)
	}
	if _, err := s.Logout(t.Context(), &v1.LogoutRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
}

func TestRegister(t *testing.T) {
	red := &fakeRedeemer{}
	var mailed []string
	s := &AuthServer{Invites: red, VerifyEmail: func(_ context.Context, u domain.User) error { mailed = append(mailed, u.Email); return nil }}

	_, err := s.Register(t.Context(), &v1.RegisterRequest{Username: "alice", Password: "pw"})
	if connectrpc.CodeOf(err) != connectrpc.CodeFailedPrecondition {
		t.Fatalf("no invite: %v", err)
	}
	code := " INV-1 "
	res, err := s.Register(t.Context(), &v1.RegisterRequest{Username: "alice", Email: "a@x.test", Password: "pw", DisplayName: "Alice", InviteCode: &code})
	if err != nil {
		t.Fatal(err)
	}
	if red.in.Code != "INV-1" || red.in.Username != "alice" || red.in.Email != "a@x.test" || red.in.DisplayName != "Alice" {
		t.Fatalf("input %+v", red.in)
	}
	if res.GetUser().GetUsername() != "alice" || !res.GetRequiresEmailVerification() || res.Session != nil {
		t.Fatalf("response %v", res)
	}
	if len(mailed) != 1 || mailed[0] != "a@x.test" {
		t.Fatalf("verification mail %v", mailed)
	}
	red.err = domain.ErrInviteInvalid
	if _, err := s.Register(t.Context(), &v1.RegisterRequest{Username: "bob", InviteCode: &code}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("bad code: %v", err)
	}
	red.err = errors.New("username too short (min 3 chars)")
	if _, err := s.Register(t.Context(), &v1.RegisterRequest{Username: "b", InviteCode: &code}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("shape error: %v", err)
	}
	s.Invites = nil
	if _, err := s.Register(t.Context(), &v1.RegisterRequest{InviteCode: &code}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("no redeemer: %v", err)
	}
}
