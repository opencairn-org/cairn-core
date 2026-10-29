package connect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/invite"
)

// InstanceInfo is the static identity of the running instance, filled by
// package main from the build metadata and config.
type InstanceInfo struct {
	Version           string
	Commit            string
	BuildTime         time.Time // zero when unknown
	FederationEnabled bool
	OAuthEnabled      bool
	RegistrationOpen  bool
}

// InviteRedeemer creates a user from an invite code (usecase/invite.RedeemInvite).
type InviteRedeemer interface {
	Execute(ctx context.Context, in invite.RedeemInviteInput) (domain.User, error)
}

// GetInstanceInfo is public: a client's server picker calls it before login.
func (s *AuthServer) GetInstanceInfo(context.Context, *v1.GetInstanceInfoRequest) (*v1.GetInstanceInfoResponse, error) {
	out := &v1.GetInstanceInfoResponse{
		Version:           s.Instance.Version,
		Commit:            s.Instance.Commit,
		FederationEnabled: s.Instance.FederationEnabled,
		OauthEnabled:      s.Instance.OAuthEnabled,
		RegistrationOpen:  s.Instance.RegistrationOpen,
		SupportedScopes:   append([]string(nil), domain.SupportedScopes...),
	}
	if !s.Instance.BuildTime.IsZero() {
		out.BuildTime = timestamppb.New(s.Instance.BuildTime)
	}
	return out, nil
}

// GetQuota reports the caller's activity quota (same numbers as GET /api/quota).
func (s *AuthServer) GetQuota(ctx context.Context, _ *v1.GetQuotaRequest) (*v1.GetQuotaResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Quota == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "quota is not available on this instance")
	}
	st, err := s.Quota.Status(ctx, userID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, err)
	}
	return &v1.GetQuotaResponse{ActivitiesUsed: connectClampInt32(st.Used), ActivitiesLimit: connectClampInt32(st.Limit),
		Unlimited: st.Unlimited, OverLimit: st.OverLimit}, nil
}

// Logout revokes the calling cookie session, or every session of the user
// with all_devices. Bearer callers have no session to end here.
func (s *AuthServer) Logout(ctx context.Context, req *v1.LogoutRequest) (*v1.LogoutResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Sessions == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "sessions are not available on this instance")
	}
	sessionID, ok := sessionIDFromHandlerCtx(ctx)
	if !ok {
		return nil, connectrpc.NewError(connectrpc.CodeFailedPrecondition,
			"no cookie session to end: revoke OAuth tokens at /oauth/revoke, personal access tokens via RevokePersonalAccessToken")
	}
	if req.GetAllDevices() {
		if err := s.Sessions.RevokeAllForUser(ctx, userID); err != nil {
			return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("revoke sessions: %w", err))
		}
		return &v1.LogoutResponse{}, nil
	}
	if err := s.Sessions.Revoke(ctx, sessionID); err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("revoke session: %w", err))
	}
	return &v1.LogoutResponse{}, nil
}

// Register creates a local user from an invite code (the instance is
// invite-only). The caller signs in afterwards; no session is issued here.
// A self-entered email gets a verification mail when mail is configured.
func (s *AuthServer) Register(ctx context.Context, req *v1.RegisterRequest) (*v1.RegisterResponse, error) {
	if s.Invites == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "registration is not available on this instance")
	}
	code := strings.TrimSpace(req.GetInviteCode())
	if code == "" {
		return nil, connectrpc.NewError(connectrpc.CodeFailedPrecondition, "registration requires an invite code on this instance")
	}
	user, err := s.Invites.Execute(ctx, invite.RedeemInviteInput{
		Code:        code,
		Username:    req.GetUsername(),
		Email:       req.GetEmail(),
		Password:    req.GetPassword(),
		DisplayName: req.GetDisplayName(),
	})
	if err != nil {
		if errors.Is(err, domain.ErrInviteInvalid) {
			return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid or already-used invite code")
		}
		// User-shape / uniqueness errors are safe to surface verbatim.
		return nil, connectError(connectrpc.CodeInvalidArgument, err)
	}
	if !user.EmailVerified && user.Email != "" && s.VerifyEmail != nil {
		_ = s.VerifyEmail(ctx, user) // best-effort, like the REST redeem
	}
	return &v1.RegisterResponse{
		User:                      userToProto(user),
		RequiresEmailVerification: !user.EmailVerified && user.Email != "",
	}, nil
}

// Compile-time check that the quota helpers accept the shared repo shapes.
var (
	_ port.QuotaRepo   = (port.QuotaRepo)(nil)
	_ port.SessionRepo = (port.SessionRepo)(nil)
)
