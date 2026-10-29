package connect

import (
	"context"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Reimport previews: start, inspect, commit, discard.

func (s *ActivityServer) PreviewSourceReimport(ctx context.Context, req *v1.PreviewSourceReimportRequest) (*v1.PreviewSourceReimportResponse, error) {
	if s.Previews == nil {
		return nil, unavailable("reimport previews")
	}
	userID, act, src, err := s.ownedSource(ctx, req.GetActivitySourceId())
	if err != nil {
		return nil, err
	}
	prev, err := s.Previews.Start(ctx, userID, act.ID, src.ID, req.GetPreferLocalReparse())
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.PreviewSourceReimportResponse{PreviewId: prev.ID.String(), JobId: prev.JobID}, nil
}

func (s *ActivityServer) GetSourcePreview(ctx context.Context, req *v1.GetSourcePreviewRequest) (*v1.GetSourcePreviewResponse, error) {
	userID, id, err := s.previewCall(ctx, req.GetPreviewId())
	if err != nil {
		return nil, err
	}
	prev, err := s.Previews.Get(ctx, userID, id)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.GetSourcePreviewResponse{Preview: sourcePreviewToProto(prev)}, nil
}

func (s *ActivityServer) CommitSourcePreview(ctx context.Context, req *v1.CommitSourcePreviewRequest) (*v1.CommitSourcePreviewResponse, error) {
	userID, id, err := s.previewCall(ctx, req.GetPreviewId())
	if err != nil {
		return nil, err
	}
	if s.PreviewCommitter == nil {
		return nil, unavailable("reimport previews")
	}
	act, err := s.PreviewCommitter.Commit(ctx, userID, id)
	if err != nil {
		return nil, actionErr(err)
	}
	sources, err := s.Activities.ListSourcesForActivity(ctx, act.ID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, err)
	}
	return &v1.CommitSourcePreviewResponse{Activity: s.withMapImage(ctx, withGear(activityToProto(act, sources), act, s.gearNames(ctx, act)), act)}, nil
}

func (s *ActivityServer) DiscardSourcePreview(ctx context.Context, req *v1.DiscardSourcePreviewRequest) (*v1.DiscardSourcePreviewResponse, error) {
	userID, id, err := s.previewCall(ctx, req.GetPreviewId())
	if err != nil {
		return nil, err
	}
	if err := s.Previews.Discard(ctx, userID, id); err != nil {
		return nil, actionErr(err)
	}
	return &v1.DiscardSourcePreviewResponse{}, nil
}

// previewCall resolves the caller and parses the preview id.
func (s *ActivityServer) previewCall(ctx context.Context, raw string) (domain.UserID, domain.SourcePreviewID, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return domain.UserID{}, domain.SourcePreviewID{}, err
	}
	if s.Previews == nil {
		return domain.UserID{}, domain.SourcePreviewID{}, unavailable("reimport previews")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return domain.UserID{}, domain.SourcePreviewID{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad preview id")
	}
	return userID, domain.SourcePreviewID(id), nil
}

func sourcePreviewToProto(p domain.SourcePreview) *v1.SourcePreview {
	out := &v1.SourcePreview{
		Id: p.ID.String(), ActivitySourceId: p.SourceID.String(), ActivityId: p.ActivityID.String(), JobId: p.JobID,
		Status:    v1.SourcePreviewStatus_SOURCE_PREVIEW_STATUS_PENDING,
		CreatedAt: timestamppb.New(p.CreatedAt), ExpiresAt: timestamppb.New(p.ExpiresAt),
	}
	if p.Status == domain.SourcePreviewReady {
		out.Status = v1.SourcePreviewStatus_SOURCE_PREVIEW_STATUS_READY
	}
	out.SourceDiff = fieldChangesToProto(p.SourceDiff)
	out.MergedDiff = fieldChangesToProto(p.MergedDiff)
	return out
}

func fieldChangesToProto(in []domain.FieldChange) []*v1.FieldChange {
	out := make([]*v1.FieldChange, 0, len(in))
	for _, c := range in {
		out = append(out, &v1.FieldChange{Field: c.Field, Before: c.Before, After: c.After})
	}
	return out
}
