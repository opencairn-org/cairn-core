package connect

import (
	"context"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Invite management.

func (s *AdminServer) invites() error {
	if s.Invites == nil {
		return unavailable("invites")
	}
	return nil
}

func parseInviteID(raw string) (domain.InviteID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return domain.InviteID{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad invite id")
	}
	return domain.InviteID(id), nil
}

func (s *AdminServer) ListInvites(ctx context.Context, req *v1.ListInvitesRequest) (*v1.ListInvitesResponse, error) {
	if err := s.invites(); err != nil {
		return nil, err
	}
	list, err := s.Invites.List(ctx, req.GetIncludeUsed(), req.GetIncludeExpired())
	if err != nil {
		return nil, actionErr(err)
	}
	limit, offset := pageFrom(req.GetPage())
	out := &v1.ListInvitesResponse{Page: &v1.PageResponse{Total: int64(len(list))}}
	end := min(offset+limit, len(list))
	for i := max(0, min(offset, len(list))); i < end; i++ {
		out.Invites = append(out.Invites, inviteToProto(list[i], "", time.Now().UTC()))
	}
	if end < len(list) {
		out.Page.NextCursor = encodeOffset(end)
	}
	return out, nil
}

func (s *AdminServer) CreateInvite(ctx context.Context, req *v1.CreateInviteRequest) (*v1.CreateInviteResponse, error) {
	if err := s.invites(); err != nil {
		return nil, err
	}
	role, _ := userRoleFromProto(req.GetAssignedRole())
	if role == "" {
		role = domain.UserRoleUser
	}
	var expires *time.Time
	if req.ExpiresAt != nil {
		t := req.GetExpiresAt().AsTime()
		expires = &t
	}
	res, err := s.Invites.Mint(ctx, s.actor(ctx), req.GetEmail(), role, expires, req.GetSendEmail())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.CreateInviteResponse{Invite: inviteToProto(res.Invite, res.Code, time.Now().UTC())}, nil
}

func (s *AdminServer) RevokeInvite(ctx context.Context, req *v1.RevokeInviteRequest) (*v1.RevokeInviteResponse, error) {
	if err := s.invites(); err != nil {
		return nil, err
	}
	id, err := parseInviteID(req.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.Invites.Revoke(ctx, s.actor(ctx), id); err != nil {
		return nil, actionErr(err)
	}
	return &v1.RevokeInviteResponse{}, nil
}

func (s *AdminServer) ResendInvite(ctx context.Context, req *v1.ResendInviteRequest) (*v1.ResendInviteResponse, error) {
	if err := s.invites(); err != nil {
		return nil, err
	}
	id, err := parseInviteID(req.GetId())
	if err != nil {
		return nil, err
	}
	res, err := s.Invites.Resend(ctx, s.actor(ctx), id)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ResendInviteResponse{Delivered: res.Delivered}
	if res.Err != nil {
		msg := res.Err.Error()
		out.ErrorMessage = &msg
	}
	return out, nil
}

func inviteToProto(inv domain.Invite, code string, now time.Time) *v1.Invite {
	out := &v1.Invite{
		Id: inv.ID.String(), Code: code, AssignedRole: userRoleToProto(inv.AssignedRole), CreatedAt: timestamppb.New(inv.CreatedAt),
		Used: inv.UsedAt != nil || inv.RevokedAt != nil,
	}
	if code == "" {
		out.Code = inv.CodePrefix
	}
	if inv.Email != "" {
		e := inv.Email
		out.Email = &e
	}
	if inv.ExpiresAt != nil {
		out.ExpiresAt = timestamppb.New(*inv.ExpiresAt)
	}
	if inv.UsedAt != nil {
		out.UsedAt = timestamppb.New(*inv.UsedAt)
	}
	if inv.UsedByUser != nil {
		v := inv.UsedByUser.String()
		out.UsedByUserId = &v
	}
	if inv.CreatedByUser != nil {
		out.CreatedByUserId = inv.CreatedByUser.String()
	}
	_ = now
	return out
}
