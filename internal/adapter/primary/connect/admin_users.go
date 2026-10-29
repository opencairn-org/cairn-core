package connect

import (
	"context"
	"fmt"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
	"github.com/opencairn-org/cairn-core/internal/usecase/useradmin"
)

type useradminPatch = useradmin.UserPatch

func useradminCreate(req *v1.CreateUserRequest, role domain.UserRole) useradmin.CreateInput {
	return useradmin.CreateInput{
		Username: req.GetUsername(), Email: req.GetEmail(), DisplayName: req.GetDisplayName(), Role: role,
		InitialPassword: req.GetInitialPassword(), Locale: req.GetLocale(), Timezone: req.GetTimezone(),
		Units: domain.UserUnits(unitsFromProto(req.GetUnits())),
	}
}

// Admin user management and the audit log. The RequireAdminInterceptor has
// already verified the caller's role; actor identity comes from the session.

func (s *AdminServer) actor(ctx context.Context) audit.Actor {
	uid, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return audit.Actor{}
	}
	a := audit.Actor{UserID: &uid, UserAgent: requestHeader(ctx).Get("User-Agent")}
	if u, err := s.Users.GetUser(ctx, uid); err == nil {
		a.Username = u.Username
	}
	return a
}

func (s *AdminServer) userAdmin() error {
	if s.UserAdmin == nil {
		return unavailable("user administration")
	}
	return nil
}

func parseUserID(raw string) (domain.UserID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return domain.UserID{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad user id")
	}
	return domain.UserID(id), nil
}

func (s *AdminServer) GetUser(ctx context.Context, req *v1.GetUserRequest) (*v1.GetUserResponse, error) {
	if err := s.userAdmin(); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.GetId())
	if err != nil {
		return nil, err
	}
	d, err := s.UserAdmin.Get(ctx, id)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetUserResponse{
		User: userToProto(d.User), ActiveSessionCount: connectClampInt32(d.ActiveSessions),
		StorageBytesUsed: d.Footprint.StorageBytes, ActivityCount: connectClampInt32(d.Footprint.Activities),
		ExternalAccountCount: connectClampInt32(d.Footprint.ExternalAccounts),
	}
	for _, li := range d.Identities {
		out.Identities = append(out.Identities, linkedIdentityToProto(li, s.providerName(li.Provider)))
	}
	return out, nil
}

func (s *AdminServer) providerName(id string) string {
	for _, p := range s.OIDCProviders {
		if p.ID == id && p.Name != "" {
			return p.Name
		}
	}
	return id
}

func (s *AdminServer) CreateUser(ctx context.Context, req *v1.CreateUserRequest) (*v1.CreateUserResponse, error) {
	if err := s.userAdmin(); err != nil {
		return nil, err
	}
	role, _ := userRoleFromProto(req.GetRole())
	res, err := s.UserAdmin.CreateUser(ctx, s.actor(ctx), useradminCreate(req, role))
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.CreateUserResponse{User: userToProto(res.User)}
	if res.Invite != nil {
		out.Invite = &v1.Invite{
			Id: res.Invite.ID.String(), Code: res.Invite.Code, AssignedRole: userRoleToProto(res.User.Role),
			CreatedAt: timestamppb.Now(), CreatedByUserId: actorID(s.actor(ctx)),
		}
		if e := res.User.Email; e != "" {
			out.Invite.Email = &e
		}
		if res.Invite.ExpiresAt != nil {
			out.Invite.ExpiresAt = timestamppb.New(*res.Invite.ExpiresAt)
		}
	}
	return out, nil
}

func (s *AdminServer) UpdateUser(ctx context.Context, req *v1.UpdateUserRequest) (*v1.UpdateUserResponse, error) {
	if err := s.userAdmin(); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.GetId())
	if err != nil {
		return nil, err
	}
	patch, err := userPatchFromProto(req)
	if err != nil {
		return nil, err
	}
	u, err := s.UserAdmin.Update(ctx, s.actor(ctx), id, patch)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.UpdateUserResponse{User: userToProto(u)}, nil
}

func (s *AdminServer) ChangeUserRole(ctx context.Context, req *v1.ChangeUserRoleRequest) (*v1.ChangeUserRoleResponse, error) {
	if err := s.userAdmin(); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.GetId())
	if err != nil {
		return nil, err
	}
	role, ok := userRoleFromProto(req.GetRole())
	if !ok {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "role is required")
	}
	u, err := s.UserAdmin.ChangeRole(ctx, s.actor(ctx), id, role)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.ChangeUserRoleResponse{User: userToProto(u)}, nil
}

func (s *AdminServer) SetUserStatus(ctx context.Context, req *v1.SetUserStatusRequest) (*v1.SetUserStatusResponse, error) {
	if err := s.userAdmin(); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.GetId())
	if err != nil {
		return nil, err
	}
	u, err := s.UserAdmin.SetStatus(ctx, s.actor(ctx), id, userStatusFromProto(req.GetStatus()), req.GetReason())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.SetUserStatusResponse{User: userToProto(u)}, nil
}

func (s *AdminServer) DeleteUser(ctx context.Context, req *v1.DeleteUserRequest) (*v1.DeleteUserResponse, error) {
	if err := s.userAdmin(); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.GetId())
	if err != nil {
		return nil, err
	}
	counts, err := s.UserAdmin.Delete(ctx, s.actor(ctx), id, req.GetConfirmUsername(), req.GetHardDelete())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.DeleteUserResponse{
		ActivitiesDeleted: connectClampInt32(counts.Activities), ExternalAccountsDeleted: connectClampInt32(counts.ExternalAccounts),
		StorageBytesFreed: counts.StorageBytes,
	}, nil
}

func (s *AdminServer) ResetUserPassword(ctx context.Context, req *v1.ResetUserPasswordRequest) (*v1.ResetUserPasswordResponse, error) {
	if err := s.userAdmin(); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.GetId())
	if err != nil {
		return nil, err
	}
	temp, err := s.UserAdmin.ResetPassword(ctx, s.actor(ctx), id)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.ResetUserPasswordResponse{TemporaryPassword: temp}, nil
}

func (s *AdminServer) RevokeUserCredentials(ctx context.Context, req *v1.RevokeUserCredentialsRequest) (*v1.RevokeUserCredentialsResponse, error) {
	if err := s.userAdmin(); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.GetId())
	if err != nil {
		return nil, err
	}
	res, err := s.UserAdmin.RevokeCredentials(ctx, s.actor(ctx), id, req.GetRevokeSessions(), req.GetRevokePersonalAccessTokens(), req.GetRevokePasskeys())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.RevokeUserCredentialsResponse{
		SessionsRevoked: connectClampInt32(res.Sessions), PatsRevoked: connectClampInt32(res.PATs), PasskeysRevoked: connectClampInt32(res.Passkeys),
	}, nil
}

// ---- audit log ----

func (s *AdminServer) ListAuditEntries(ctx context.Context, req *v1.ListAuditEntriesRequest) (*v1.ListAuditEntriesResponse, error) {
	if s.Audit == nil {
		return nil, unavailable("the audit log")
	}
	limit, offset := pageFrom(req.GetPage())
	f := domain.AuditFilter{
		ResourceType: req.GetResourceType(), ResourceID: req.GetResourceId(), Actions: req.GetActions(),
		MinSeverity: auditSeverityFromProto(req.GetMinSeverity()), Query: req.GetQuery(), Limit: limit, Offset: offset,
	}
	f.From, f.To = timeRangeBounds(req.GetTimeRange())
	if req.ActorUserId != nil {
		uid, err := parseUserID(req.GetActorUserId())
		if err != nil {
			return nil, err
		}
		f.ActorUserID = &uid
	}
	entries, err := s.Audit.List(ctx, f)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListAuditEntriesResponse{Page: &v1.PageResponse{Total: -1}}
	for _, e := range entries {
		out.Entries = append(out.Entries, auditEntryToProto(e))
	}
	if len(entries) == limit {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

func auditEntryToProto(e domain.AuditEntry) *v1.AuditEntry {
	out := &v1.AuditEntry{
		Id: e.ID.String(), Ts: timestamppb.New(e.At), Action: e.Action, Severity: auditSeverityToProto(e.Severity), Metadata: e.Metadata,
	}
	if e.ActorUserID != nil {
		v := e.ActorUserID.String()
		out.ActorUserId = &v
	}
	if e.ActorUsername != "" {
		v := e.ActorUsername
		out.ActorUsername = &v
	}
	if e.ActorPATID != nil {
		v := e.ActorPATID.String()
		out.ActorPatId = &v
	}
	if e.IPAddress != "" {
		v := e.IPAddress
		out.IpAddress = &v
	}
	if e.ResourceType != "" {
		v := e.ResourceType
		out.ResourceType = &v
	}
	if e.ResourceID != "" {
		v := e.ResourceID
		out.ResourceId = &v
	}
	if len(e.Diff) > 0 {
		fields := map[string]any{}
		for _, c := range e.Diff {
			fields[c.Field] = map[string]any{"before": c.Before, "after": c.After}
		}
		out.Diff, _ = structpb.NewStruct(fields)
	}
	return out
}

func auditSeverityToProto(s domain.AuditSeverity) v1.AuditSeverity {
	switch s {
	case domain.AuditWarn:
		return v1.AuditSeverity_AUDIT_SEVERITY_WARN
	case domain.AuditHigh:
		return v1.AuditSeverity_AUDIT_SEVERITY_HIGH
	case domain.AuditInfo:
		return v1.AuditSeverity_AUDIT_SEVERITY_INFO
	}
	return v1.AuditSeverity_AUDIT_SEVERITY_UNSPECIFIED
}

func auditSeverityFromProto(s v1.AuditSeverity) domain.AuditSeverity {
	switch s {
	case v1.AuditSeverity_AUDIT_SEVERITY_INFO:
		return domain.AuditInfo
	case v1.AuditSeverity_AUDIT_SEVERITY_WARN:
		return domain.AuditWarn
	case v1.AuditSeverity_AUDIT_SEVERITY_HIGH:
		return domain.AuditHigh
	}
	return ""
}

// ---- converters ----

func actorID(a audit.Actor) string {
	if a.UserID == nil {
		return ""
	}
	return a.UserID.String()
}

func userRoleFromProto(r v1.UserRole) (domain.UserRole, bool) {
	switch r {
	case v1.UserRole_USER_ROLE_USER:
		return domain.UserRoleUser, true
	case v1.UserRole_USER_ROLE_ADMIN:
		return domain.UserRoleAdmin, true
	}
	return "", false
}

func userStatusFromProto(s v1.UserStatus) domain.UserStatus {
	switch s {
	case v1.UserStatus_USER_STATUS_ACTIVE:
		return domain.UserStatusActive
	case v1.UserStatus_USER_STATUS_INVITED:
		return domain.UserStatusInvited
	case v1.UserStatus_USER_STATUS_SUSPENDED:
		return domain.UserStatusSuspended
	case v1.UserStatus_USER_STATUS_DELETED:
		return domain.UserStatusDeleted
	}
	return ""
}

func userPatchFromProto(req *v1.UpdateUserRequest) (useradminPatch, error) {
	mask := map[string]bool{}
	for _, f := range req.GetFieldMask() {
		switch f {
		case "email", "display_name", "locale", "timezone", "units":
			mask[f] = true
		default:
			return useradminPatch{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, fmt.Sprintf("unknown field_mask entry %q", f))
		}
	}
	in := func(f string) bool { return len(mask) == 0 || mask[f] }
	var p useradminPatch
	if in("email") {
		p.Email = req.Email
	}
	if in("display_name") {
		p.DisplayName = req.DisplayName
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
