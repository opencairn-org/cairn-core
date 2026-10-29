package connect

import (
	"context"
	"slices"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
)

// Self-service account RPCs. Each one delegates to usecase/account; the
// REST handlers under /api/pats, /api/sessions, /api/identities,
// /api/profile and /auth/password/* use the same use cases.

func unavailable(what string) error {
	return connectrpc.NewError(connectrpc.CodeUnavailable, what+" are not available on this instance")
}

// ---- personal access tokens ----

func (s *AuthServer) CreatePersonalAccessToken(ctx context.Context, req *v1.CreatePersonalAccessTokenRequest) (*v1.CreatePersonalAccessTokenResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.PATs == nil {
		return nil, unavailable("personal access tokens")
	}
	var expires *time.Time
	if req.ExpiresAt != nil {
		t := req.GetExpiresAt().AsTime()
		expires = &t
	}
	pat, secret, err := s.PATs.Create(ctx, userID, req.GetName(), req.GetScopes(), expires)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.CreatePersonalAccessTokenResponse{Token: patToProto(pat), Secret: secret}, nil
}

func (s *AuthServer) ListPersonalAccessTokens(ctx context.Context, _ *v1.ListPersonalAccessTokensRequest) (*v1.ListPersonalAccessTokensResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.PATs == nil {
		return nil, unavailable("personal access tokens")
	}
	list, err := s.PATs.List(ctx, userID)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListPersonalAccessTokensResponse{}
	for _, p := range list {
		if p.RevokedAt != nil {
			continue // the list is the live set; revoked tokens are gone
		}
		out.Tokens = append(out.Tokens, patToProto(p))
	}
	return out, nil
}

func (s *AuthServer) RevokePersonalAccessToken(ctx context.Context, req *v1.RevokePersonalAccessTokenRequest) (*v1.RevokePersonalAccessTokenResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.PATs == nil {
		return nil, unavailable("personal access tokens")
	}
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad token id")
	}
	if err := s.PATs.Revoke(ctx, userID, domain.PATID(id)); err != nil {
		return nil, actionErr(err)
	}
	return &v1.RevokePersonalAccessTokenResponse{}, nil
}

func patToProto(p domain.PersonalAccessToken) *v1.PersonalAccessToken {
	out := &v1.PersonalAccessToken{
		Id: p.ID.String(), Name: p.Name, Scopes: p.Scopes, CreatedAt: timestamppb.New(p.CreatedAt),
	}
	if p.ExpiresAt != nil {
		out.ExpiresAt = timestamppb.New(*p.ExpiresAt)
	}
	if p.LastUsedAt != nil {
		out.LastUsedAt = timestamppb.New(*p.LastUsedAt)
	}
	return out
}

// ---- sessions ----

func (s *AuthServer) ListSessions(ctx context.Context, _ *v1.ListSessionsRequest) (*v1.ListSessionsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.SessionMgmt == nil {
		return nil, unavailable("sessions")
	}
	current, _ := sessionIDFromHandlerCtx(ctx)
	list, err := s.SessionMgmt.List(ctx, userID)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListSessionsResponse{}
	for _, sess := range list {
		out.Sessions = append(out.Sessions, sessionToProto(sess, sess.ID == current))
	}
	return out, nil
}

func (s *AuthServer) RevokeSession(ctx context.Context, req *v1.RevokeSessionRequest) (*v1.RevokeSessionResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.SessionMgmt == nil {
		return nil, unavailable("sessions")
	}
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad session id")
	}
	if err := s.SessionMgmt.Revoke(ctx, userID, domain.SessionID(id)); err != nil {
		return nil, actionErr(err)
	}
	return &v1.RevokeSessionResponse{}, nil
}

func sessionToProto(sess domain.Session, current bool) *v1.Session {
	out := &v1.Session{
		Id:               uuid.UUID(sess.ID).String(),
		UserAgentSummary: sess.UserAgentSummary,
		CreatedAt:        timestamppb.New(sess.CreatedAt),
		LastSeenAt:       timestamppb.New(sess.LastSeenAt),
		ExpiresAt:        timestamppb.New(sess.ExpiresAt),
		IsCurrent:        current,
		AuthMethod:       authMethodToProto(sess.AuthMethod),
	}
	if sess.IPAddress.IsValid() {
		out.IpAddress = sess.IPAddress.String()
	}
	if sess.IPGeoSummary != "" {
		geo := sess.IPGeoSummary
		out.IpGeoSummary = &geo
	}
	return out
}

func authMethodToProto(m domain.SessionAuthMethod) v1.AuthMethod {
	switch m {
	case domain.SessionAuthPassword:
		return v1.AuthMethod_AUTH_METHOD_PASSWORD
	case domain.SessionAuthPasswordWithWebauthn:
		return v1.AuthMethod_AUTH_METHOD_PASSWORD_WITH_WEBAUTHN_2FA
	case domain.SessionAuthPasskey:
		return v1.AuthMethod_AUTH_METHOD_WEBAUTHN_PASSKEY
	case domain.SessionAuthOIDC:
		return v1.AuthMethod_AUTH_METHOD_OIDC
	case domain.SessionAuthPAT:
		return v1.AuthMethod_AUTH_METHOD_PERSONAL_ACCESS_TOKEN
	}
	return v1.AuthMethod_AUTH_METHOD_UNSPECIFIED
}

// ---- passwords ----

func (s *AuthServer) ChangePassword(ctx context.Context, req *v1.ChangePasswordRequest) (*v1.ChangePasswordResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Passwords == nil {
		return nil, unavailable("passwords")
	}
	keep, _ := sessionIDFromHandlerCtx(ctx)
	if err := s.Passwords.Change(ctx, userID, keep, req.GetCurrentPassword(), req.GetNewPassword()); err != nil {
		return nil, actionErr(err)
	}
	return &v1.ChangePasswordResponse{}, nil
}

func (s *AuthServer) RequestPasswordReset(ctx context.Context, req *v1.RequestPasswordResetRequest) (*v1.RequestPasswordResetResponse, error) {
	if s.Passwords == nil {
		return &v1.RequestPasswordResetResponse{}, nil
	}
	attempted, err := s.Passwords.RequestReset(ctx, req.GetEmail())
	if err != nil && s.Log != nil {
		s.Log.Warn("password reset mail", "error", err) // never surfaced: anti-enumeration
	}
	return &v1.RequestPasswordResetResponse{EmailDispatchAttempted: attempted}, nil
}

func (s *AuthServer) ConfirmPasswordReset(ctx context.Context, req *v1.ConfirmPasswordResetRequest) (*v1.ConfirmPasswordResetResponse, error) {
	if s.Passwords == nil {
		return nil, unavailable("passwords")
	}
	if _, err := s.Passwords.ConfirmReset(ctx, req.GetResetToken(), req.GetNewPassword()); err != nil {
		return nil, actionErr(err)
	}
	return &v1.ConfirmPasswordResetResponse{}, nil
}

// ---- linked identities ----

func (s *AuthServer) ListLinkedIdentities(ctx context.Context, _ *v1.ListLinkedIdentitiesRequest) (*v1.ListLinkedIdentitiesResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Identities == nil {
		return nil, unavailable("linked identities")
	}
	list, err := s.Identities.List(ctx, userID)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListLinkedIdentitiesResponse{}
	for _, li := range list {
		out.Identities = append(out.Identities, linkedIdentityToProto(li, s.Identities.ProviderName(li.Provider)))
	}
	return out, nil
}

func (s *AuthServer) UnlinkOIDCIdentity(ctx context.Context, req *v1.UnlinkOIDCIdentityRequest) (*v1.UnlinkOIDCIdentityResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Identities == nil {
		return nil, unavailable("linked identities")
	}
	id, err := uuid.Parse(req.GetIdentityId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad identity id")
	}
	if err := s.Identities.Unlink(ctx, userID, domain.LinkedIdentityID(id)); err != nil {
		return nil, actionErr(err)
	}
	return &v1.UnlinkOIDCIdentityResponse{}, nil
}

func linkedIdentityToProto(li domain.LinkedIdentity, providerName string) *v1.LinkedIdentity {
	out := &v1.LinkedIdentity{
		Id:                    uuid.UUID(li.ID).String(),
		OidcClientId:          li.Provider,
		OidcClientDisplayName: providerName,
		Subject:               li.Subject,
		LinkedAt:              timestamppb.New(li.LinkedAt),
	}
	if li.Email != "" {
		e := li.Email
		out.Email = &e
	}
	if li.LastUsedAt != nil {
		out.LastUsedAt = timestamppb.New(*li.LastUsedAt)
	}
	return out
}

// ---- profile ----

func (s *AuthServer) UpdateProfile(ctx context.Context, req *v1.UpdateProfileRequest) (*v1.UpdateProfileResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Profiles == nil {
		return nil, unavailable("profiles")
	}
	patch, err := profilePatchFromProto(req)
	if err != nil {
		return nil, err
	}
	u, err := s.Profiles.Update(ctx, userID, patch)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.UpdateProfileResponse{User: userToProto(u)}, nil
}

// profilePatchFromProto applies field_mask; an empty mask means "every set field".
func profilePatchFromProto(req *v1.UpdateProfileRequest) (account.ProfilePatch, error) {
	mask := req.GetFieldMask()
	if err := checkProfileMask(mask, req.Email != nil); err != nil {
		return account.ProfilePatch{}, err
	}
	in := func(name string) bool { return len(mask) == 0 || slices.Contains(mask, name) }
	var p account.ProfilePatch
	if in("display_name") {
		p.DisplayName = req.DisplayName
	}
	if in("avatar_url") {
		p.AvatarURL = req.AvatarUrl
	}
	if in("locale") {
		p.Locale = req.Locale
	}
	if in("timezone") {
		p.Timezone = req.Timezone
	}
	if in("units") && req.Units != nil {
		u := unitsFromProto(req.GetUnits())
		p.Units = &u
	}
	return p, nil
}

// checkProfileMask refuses unknown entries and any attempt to change the email.
func checkProfileMask(mask []string, emailSet bool) error {
	if slices.Contains(mask, "email") || (len(mask) == 0 && emailSet) {
		return connectrpc.NewError(connectrpc.CodeInvalidArgument, account.ErrEmailChangeUnsupported.Error())
	}
	for _, f := range mask {
		switch f {
		case "display_name", "avatar_url", "locale", "timezone", "units":
		default:
			return connectrpc.NewError(connectrpc.CodeInvalidArgument, "unknown field_mask entry "+f)
		}
	}
	return nil
}
