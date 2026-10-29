package connect

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Authorized apps: the OAuth clients holding live tokens for the caller.

func (s *AuthServer) ListAuthorizedApps(ctx context.Context, _ *v1.ListAuthorizedAppsRequest) (*v1.ListAuthorizedAppsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Apps == nil {
		return nil, unavailable("authorized apps")
	}
	grants, err := s.Apps.List(ctx, userID)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListAuthorizedAppsResponse{}
	for _, g := range grants {
		out.Apps = append(out.Apps, authorizedAppToProto(g))
	}
	return out, nil
}

func (s *AuthServer) RevokeAuthorizedApp(ctx context.Context, req *v1.RevokeAuthorizedAppRequest) (*v1.RevokeAuthorizedAppResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Apps == nil {
		return nil, unavailable("authorized apps")
	}
	n, err := s.Apps.Revoke(ctx, userID, req.GetClientId())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.RevokeAuthorizedAppResponse{TokensRevoked: connectClampInt32(n)}, nil
}

func authorizedAppToProto(g domain.OAuthGrant) *v1.AuthorizedApp {
	return &v1.AuthorizedApp{
		ClientId: g.ClientID, ClientName: g.ClientName, SoftwareId: g.SoftwareID, SoftwareVersion: g.SoftwareVersion,
		Scopes: g.Scopes, GrantedAt: timestamppb.New(g.GrantedAt), LastIssuedAt: timestamppb.New(g.LastIssuedAt),
		ActiveTokens: connectClampInt32(g.ActiveTokens),
	}
}
