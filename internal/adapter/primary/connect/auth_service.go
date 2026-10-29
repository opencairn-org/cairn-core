package connect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/account"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
)

// AuthServer implements cairnv1connect.AuthServiceHandler.
//
// Browser flows (password login, OIDC start/link, passkeys) stay on the REST
// handlers because they are redirect- and cookie-driven; the proto comments
// on those RPCs say so and they return CodeUnimplemented. Everything else
// falls through to the embedded Unimplemented base until implemented.
type AuthServer struct {
	cairnv1connect.UnimplementedAuthServiceHandler

	Users port.UserRepo
	// OIDCProviders are the env-configured (CAIRN_OIDC_*) providers. Immutable
	// at runtime — there is no client database. Empty means OIDC is disabled.
	OIDCProviders []domain.OIDCProvider

	// Instance backs GetInstanceInfo.
	Instance InstanceInfo
	// Sessions backs Logout; nil → Unavailable.
	Sessions port.SessionRepo
	// Activities + Quotas + QuotaMaxActivities back GetQuota (same rule as
	// GET /api/quota: per-user override, else the instance default, 0 = unlimited).
	Activities         port.ActivityRepo
	Quotas             port.QuotaRepo
	QuotaMaxActivities int
	// Invites backs Register; VerifyEmail (optional) mails a verification link
	// for a self-entered address.
	Invites     InviteRedeemer
	VerifyEmail func(ctx context.Context, u domain.User) error

	// Self-service account use cases (nil → Unavailable on their RPCs).
	PATs        *account.Tokens
	SessionMgmt *account.Sessions
	Passwords   *account.Passwords
	Identities  *account.Identities
	Profiles    *account.Profiles
	Apps        *account.AuthorizedApps
	// SecondFactor backs Complete2FA and SetPasskeySecondFactor; SessionTTL
	// bounds the bearer sessions Complete2FA issues.
	SecondFactor *authuc.SecondFactor
	SessionTTL   time.Duration
	// Log receives failures that must not reach the caller (reset mail).
	Log *slog.Logger
}

var _ cairnv1connect.AuthServiceHandler = (*AuthServer)(nil)

func NewAuthServer(users port.UserRepo, providers []domain.OIDCProvider) *AuthServer {
	return &AuthServer{Users: users, OIDCProviders: providers}
}

// ---------------------------------------------------------------------------
// GetCurrentUser
//
// The "who am I?" call. The session interceptor stashes the user id on
// successful cookie verification; without a session this returns
// Unauthenticated and the frontend redirects to /login.
// ---------------------------------------------------------------------------

func (s *AuthServer) GetCurrentUser(
	ctx context.Context,
	req *v1.GetCurrentUserRequest,
) (*v1.GetCurrentUserResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}

	u, err := s.Users.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Session points at a user that's been hard-deleted. Treat
			// as unauthenticated rather than 500 — the cookie will be
			// invalidated by the next /auth/logout or expire naturally.
			return nil, connectrpc.NewError(connectrpc.CodeUnauthenticated,
				"user not found")
		}
		return nil, connectError(connectrpc.CodeInternal,
			fmt.Errorf("get user: %w", err))
	}

	perms := []string{"user"}
	if u.Role == domain.UserRoleAdmin {
		perms = append(perms, "admin")
	}
	if u.Role.CanModerate() {
		perms = append(perms, "moderate")
	}

	return &v1.GetCurrentUserResponse{
		User:        userToProto(u),
		Permissions: perms,
	}, nil
}

// ---------------------------------------------------------------------------
// GetLoginMethods
//
// Pre-login probe: lists which sign-in options the instance offers.
// Used by the /login page to decide what buttons to render.
//
// `username_or_email` in the request is currently ignored — every
// instance-wide method is returned. When per-user method gating ships
// (e.g. SSO-only org members), this branches on that input.
// ---------------------------------------------------------------------------

func (s *AuthServer) GetLoginMethods(
	ctx context.Context,
	req *v1.GetLoginMethodsRequest,
) (*v1.GetLoginMethodsResponse, error) {
	out := &v1.GetLoginMethodsResponse{
		// Password / passkey availability is global today. Once
		// per-user gating ships, branch on the request's hint.
		PasswordEnabled:  true,
		WebauthnEnabled:  true,  // passkey login via /auth/webauthn/* REST endpoints
		RegistrationOpen: false, // invite-only by default
		RequiresInvite:   true,
	}
	for _, p := range s.OIDCProviders {
		out.OidcClients = append(out.OidcClients, &v1.OIDCLoginOption{
			Id:          p.ID,
			DisplayName: p.Name,
		})
	}
	return out, nil
}

// userToProto converts the domain User into the proto wire type.
func userToProto(u domain.User) *v1.User {
	out := &v1.User{
		Id:                  u.ID.String(),
		Username:            u.Username,
		Email:               u.Email,
		EmailVerified:       u.EmailVerified,
		DisplayName:         u.DisplayName,
		Locale:              u.Locale,
		Timezone:            u.Timezone,
		Units:               unitsToProto(u.Units),
		DateFormat:          dateFormatToProto(u.DateFormat),
		TimeFormat:          timeFormatToProto(u.TimeFormat),
		PasskeySecondFactor: u.PasskeySecondFactor,
		Role:                userRoleToProto(u.Role),
		Status:              userStatusToProto(u.Status),
		CreatedAt:           timestamppb.New(u.CreatedAt),
		UpdatedAt:           timestamppb.New(u.UpdatedAt),
	}
	if u.AvatarURL != "" {
		v := u.AvatarURL
		out.AvatarUrl = &v
	}
	if u.LastLoginAt != nil {
		out.LastLoginAt = timestamppb.New(*u.LastLoginAt)
	}
	return out
}

func userRoleToProto(r domain.UserRole) v1.UserRole {
	switch r {
	case domain.UserRoleUser:
		return v1.UserRole_USER_ROLE_USER
	case domain.UserRoleAdmin:
		return v1.UserRole_USER_ROLE_ADMIN
	}
	return v1.UserRole_USER_ROLE_UNSPECIFIED
}

func userStatusToProto(s domain.UserStatus) v1.UserStatus {
	switch s {
	case domain.UserStatusActive:
		return v1.UserStatus_USER_STATUS_ACTIVE
	case domain.UserStatusInvited:
		return v1.UserStatus_USER_STATUS_INVITED
	case domain.UserStatusSuspended:
		return v1.UserStatus_USER_STATUS_SUSPENDED
	case domain.UserStatusDeleted:
		return v1.UserStatus_USER_STATUS_DELETED
	}
	return v1.UserStatus_USER_STATUS_UNSPECIFIED
}

// ---- Format-preference enum <-> domain string converters ----

func unitsToProto(u domain.UserUnits) v1.UnitSystem {
	switch u {
	case domain.UserUnitsMetric:
		return v1.UnitSystem_UNIT_SYSTEM_METRIC
	case domain.UserUnitsImperial:
		return v1.UnitSystem_UNIT_SYSTEM_IMPERIAL
	}
	return v1.UnitSystem_UNIT_SYSTEM_UNSPECIFIED
}

func unitsFromProto(u v1.UnitSystem) string {
	switch u {
	case v1.UnitSystem_UNIT_SYSTEM_METRIC:
		return string(domain.UserUnitsMetric)
	case v1.UnitSystem_UNIT_SYSTEM_IMPERIAL:
		return string(domain.UserUnitsImperial)
	}
	return ""
}

func dateFormatToProto(s string) v1.DateFormat {
	switch s {
	case "iso":
		return v1.DateFormat_DATE_FORMAT_ISO
	case "us":
		return v1.DateFormat_DATE_FORMAT_US
	case "eu":
		return v1.DateFormat_DATE_FORMAT_EU
	}
	return v1.DateFormat_DATE_FORMAT_UNSPECIFIED
}

func dateFormatFromProto(d v1.DateFormat) string {
	switch d {
	case v1.DateFormat_DATE_FORMAT_ISO:
		return "iso"
	case v1.DateFormat_DATE_FORMAT_US:
		return "us"
	case v1.DateFormat_DATE_FORMAT_EU:
		return "eu"
	}
	return ""
}

func timeFormatToProto(s string) v1.TimeFormat {
	switch s {
	case "24h":
		return v1.TimeFormat_TIME_FORMAT_24H
	case "12h":
		return v1.TimeFormat_TIME_FORMAT_12H
	}
	return v1.TimeFormat_TIME_FORMAT_UNSPECIFIED
}

func timeFormatFromProto(t v1.TimeFormat) string {
	switch t {
	case v1.TimeFormat_TIME_FORMAT_24H:
		return "24h"
	case v1.TimeFormat_TIME_FORMAT_12H:
		return "12h"
	}
	return ""
}

// UpdateUserPreferences persists the caller's display-format preferences and
// returns the updated user. Session-gated (userIDFromHandlerCtx).
func (s *AuthServer) UpdateUserPreferences(
	ctx context.Context,
	req *v1.UpdateUserPreferencesRequest,
) (*v1.UpdateUserPreferencesResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	m := req
	if err := s.Users.UpdateUserPreferences(ctx, userID,
		unitsFromProto(m.GetUnits()),
		dateFormatFromProto(m.GetDateFormat()),
		timeFormatFromProto(m.GetTimeFormat()),
	); err != nil {
		return nil, connectError(connectrpc.CodeInternal,
			fmt.Errorf("update preferences: %w", err))
	}
	u, err := s.Users.GetUser(ctx, userID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("reload user: %w", err))
	}
	return &v1.UpdateUserPreferencesResponse{User: userToProto(u)}, nil
}
