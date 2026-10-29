package connect

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
	"github.com/opencairn-org/cairn-core/internal/usecase/fleet"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
	"github.com/opencairn-org/cairn-core/internal/usecase/invite"
	"github.com/opencairn-org/cairn-core/internal/usecase/jobs"
	"github.com/opencairn-org/cairn-core/internal/usecase/maintenance"
	"github.com/opencairn-org/cairn-core/internal/usecase/moderation"
	"github.com/opencairn-org/cairn-core/internal/usecase/useradmin"
	"github.com/opencairn-org/cairn-core/internal/usecase/webhooks"
)

// AdminServer implements cairnv1connect.AdminServiceHandler.
//
// OIDC providers are configured via CAIRN_OIDC_* env vars, so the admin
// surface is read-only — there is no create/update/delete.
//
// Authorisation: all RPCs require the caller to have `admin` role. The
// check is performed by RequireAdminInterceptor when this service is
// mounted; handlers themselves only do the standard userIDFromHandlerCtx
// for traceability — the interceptor has already filtered non-admins.
type AdminServer struct {
	cairnv1connect.UnimplementedAdminServiceHandler

	Users         port.UserRepo
	OIDCProviders []domain.OIDCProvider

	// Use cases behind the RPC groups; a nil one answers Unavailable.
	UserAdmin *useradmin.Users
	Audit     *audit.Recorder
	Settings  *instance.Settings
	TestEmail *instance.TestEmail
	Health    *instance.Health
	Invites   *invite.Admin
	Jobs      *jobs.Jobs
	Orphans   *jobs.Orphans
	Fleet     *fleet.Fleet
	// CoreRelease is this binary's release, reported next to each worker.
	CoreRelease   string
	Reports       *moderation.Reports
	Blocks        *moderation.Blocks
	MergeDefaults *instance.MergeDefaults
	Ops           *instance.Ops
	WebhookTokens *webhooks.Tokens
	// Events is the live admin channel behind StreamAdminEvents.
	Events      *AdminEventHub
	Maintenance *maintenance.Runner
}

var _ cairnv1connect.AdminServiceHandler = (*AdminServer)(nil)

func NewAdminServer(users port.UserRepo, providers []domain.OIDCProvider) *AdminServer {
	return &AdminServer{Users: users, OIDCProviders: providers}
}

// ---------------------------------------------------------------------------
// ListUsers
// ---------------------------------------------------------------------------

func (s *AdminServer) ListUsers(
	ctx context.Context,
	req *v1.ListUsersRequest,
) (*v1.ListUsersResponse, error) {
	if s.UserAdmin == nil {
		return nil, unavailable("user administration")
	}
	limit, _ := pageFrom(req.GetPage())
	cursor := ""
	if p := req.GetPage(); p != nil {
		cursor = p.GetCursor()
	}
	rows, nextCursor, err := s.UserAdmin.Page(ctx, cursor, limit)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListUsersResponse{Users: make([]*v1.AdminUserSummary, 0, len(rows)), Page: &v1.PageResponse{NextCursor: nextCursor, Total: -1}}
	for _, r := range rows {
		fp := r.Footprint
		row := &v1.AdminUserSummary{User: userToProto(r.User), ActivityCount: connectClampInt32(fp.Activities),
			ExternalAccountCount: connectClampInt32(fp.ExternalAccounts), StorageBytesUsed: fp.StorageBytes}
		if fp.LastActivityAt != nil {
			row.LastActivityAt = timestamppb.New(*fp.LastActivityAt)
		}
		out.Users = append(out.Users, row)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// ListOIDCClients
// ---------------------------------------------------------------------------

func (s *AdminServer) ListOIDCClients(
	ctx context.Context,
	req *v1.ListOIDCClientsRequest,
) (*v1.ListOIDCClientsResponse, error) {
	out := &v1.ListOIDCClientsResponse{}
	for _, p := range s.OIDCProviders {
		out.Clients = append(out.Clients, oidcProviderToProto(p))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func oidcProviderToProto(p domain.OIDCProvider) *v1.OIDCClient {
	return &v1.OIDCClient{
		Id:                p.ID,
		DisplayName:       p.Name,
		IssuerUrl:         p.IssuerURL,
		ClientId:          p.ClientID,
		HasClientSecret:   p.ClientSecret != "",
		Scopes:            p.Scopes,
		UsePkce:           p.UsePKCE,
		Audience:          p.Audience,
		SkipAudienceCheck: p.SkipAudienceCheck,
		AutoProvision:     p.AutoProvision,
		AutoProvisionRole: userRoleToProto(p.AutoProvisionRole),
	}
}
