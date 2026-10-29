package connect

import (
	"context"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

func (s *AdminServer) ListWebhookSigningTokens(ctx context.Context, req *v1.ListWebhookSigningTokensRequest) (*v1.ListWebhookSigningTokensResponse, error) {
	if s.WebhookTokens == nil {
		return nil, unavailable("webhook tokens")
	}
	limit, offset := pageFrom(req.GetPage())
	rows, total, err := s.WebhookTokens.List(ctx, req.GetProvider(), limit, offset)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListWebhookSigningTokensResponse{Page: &v1.PageResponse{Total: int64(total)}}
	for _, row := range rows {
		info := &v1.WebhookSigningTokenInfo{ExternalAccountId: row.ExternalAccountID.String(), UserId: row.UserID.String(), Provider: row.Provider}
		if row.RotatedAt != nil {
			info.LastRotatedAt = timestamppb.New(*row.RotatedAt)
		}
		out.Tokens = append(out.Tokens, info)
	}
	if offset+len(rows) < total {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

func (s *AdminServer) BulkRotateWebhookSigningTokens(ctx context.Context, req *v1.BulkRotateWebhookSigningTokensRequest) (*v1.BulkRotateWebhookSigningTokensResponse, error) {
	if s.WebhookTokens == nil {
		return nil, unavailable("webhook tokens")
	}
	sel := domain.WebhookRotationSelector{Provider: req.GetProvider()}
	if req.OlderThan != nil {
		d := req.GetOlderThan().AsDuration()
		sel.OlderThan = &d
	}
	for _, raw := range req.GetExternalAccountIds() {
		id, err := parseExternalAccountID(raw)
		if err != nil {
			return nil, err
		}
		sel.AccountIDs = append(sel.AccountIDs, id)
	}
	rotated, err := s.WebhookTokens.BulkRotate(ctx, s.actor(ctx), sel)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.BulkRotateWebhookSigningTokensResponse{RotatedCount: connectClampInt32(len(rotated))}
	for _, id := range rotated {
		out.RotationJobs = append(out.RotationJobs, &v1.WebhookRotationJobs{ExternalAccountId: id.String()})
	}
	return out, nil
}

func parseExternalAccountID(raw string) (domain.ExternalAccountID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return domain.ExternalAccountID{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad external account id")
	}
	return domain.ExternalAccountID(id), nil
}
