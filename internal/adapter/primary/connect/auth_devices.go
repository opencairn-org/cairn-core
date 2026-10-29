package connect

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Device authorization (RFC 8628): a signed-in client approving the sign-in of
// another device. The scope interceptor requires devices:read / devices:write
// of OAuth callers; the use case caps what such a caller can grant.

func (s *AuthServer) LookupDeviceAuthorization(ctx context.Context, req *v1.LookupDeviceAuthorizationRequest) (*v1.LookupDeviceAuthorizationResponse, error) {
	if _, err := userIDFromHandlerCtx(ctx); err != nil {
		return nil, err
	}
	if s.Devices == nil {
		return nil, unavailable("device authorization")
	}
	pending, err := s.Devices.Lookup(ctx, req.GetUserCode(), scopesFromHandlerCtx(ctx))
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.LookupDeviceAuthorizationResponse{
		ClientId: pending.Client.ClientID, ClientName: pending.Client.Name,
		SoftwareId: pending.Client.SoftwareID, SoftwareVersion: pending.Client.SoftwareVersion,
		ExpiresAt: timestamppb.New(pending.Code.ExpiresAt),
	}
	for _, sc := range pending.Scopes {
		out.Scopes = append(out.Scopes, &v1.DeviceAuthorizationScope{
			Scope: sc, Description: domain.ScopeDescription(sc), Write: domain.IsWriteScope(sc),
		})
	}
	return out, nil
}

func (s *AuthServer) DecideDeviceAuthorization(ctx context.Context, req *v1.DecideDeviceAuthorizationRequest) (*v1.DecideDeviceAuthorizationResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Devices == nil {
		return nil, unavailable("device authorization")
	}
	granted, err := s.Devices.Decide(ctx, userID, req.GetUserCode(), req.GetApprove(), scopesFromHandlerCtx(ctx))
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.DecideDeviceAuthorizationResponse{GrantedScopes: granted}, nil
}
