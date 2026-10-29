package connect

import (
	"context"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Custom best-effort windows: create, list, delete. Backfill after a create
// runs in the background through s.Backfill (set by package main).

func (s *ActivityServer) CreateCustomBestEffort(ctx context.Context, req *v1.CreateCustomBestEffortRequest) (*v1.CreateCustomBestEffortResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Customs == nil {
		return nil, unavailable("custom best efforts")
	}
	at, ok := activityTypeFromProto(req.GetActivityType())
	if !ok {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "activity_type is required")
	}
	metric, _ := bestEffortMetricFromProto(req.GetMetric())
	created, err := s.Customs.Create(ctx, domain.CustomBestEffort{
		UserID: userID, ActivityType: at, Metric: metric,
		WindowKind:  bestEffortWindowKindFromProto(req.GetWindowKind()),
		WindowValue: int(req.GetWindowValue()),
		Label:       req.GetLabel(),
	})
	if err != nil {
		return nil, actionErr(err)
	}
	if s.Backfill != nil {
		s.Backfill(userID, at)
	}
	return &v1.CreateCustomBestEffortResponse{CustomBestEffort: customBestEffortToProto(created)}, nil
}

func (s *ActivityServer) ListCustomBestEfforts(ctx context.Context, _ *v1.ListCustomBestEffortsRequest) (*v1.ListCustomBestEffortsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Customs == nil {
		return nil, unavailable("custom best efforts")
	}
	list, err := s.Customs.List(ctx, userID)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListCustomBestEffortsResponse{}
	for _, c := range list {
		out.CustomBestEfforts = append(out.CustomBestEfforts, customBestEffortToProto(c))
	}
	return out, nil
}

func (s *ActivityServer) DeleteCustomBestEffort(ctx context.Context, req *v1.DeleteCustomBestEffortRequest) (*v1.DeleteCustomBestEffortResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Customs == nil {
		return nil, unavailable("custom best efforts")
	}
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad id")
	}
	if err := s.Customs.Delete(ctx, userID, domain.CustomBestEffortID(id)); err != nil {
		return nil, actionErr(err)
	}
	return &v1.DeleteCustomBestEffortResponse{}, nil
}

func customBestEffortToProto(c domain.CustomBestEffort) *v1.CustomBestEffort {
	return &v1.CustomBestEffort{
		Id: c.ID.String(), UserId: c.UserID.String(),
		ActivityType: activityTypeToProto(c.ActivityType), Metric: bestEffortMetricToProto(c.Metric),
		WindowKind: bestEffortWindowKindToProto(c.WindowKind), WindowValue: connectClampInt32(c.WindowValue),
		Label: c.Label, CreatedAt: timestamppb.New(c.CreatedAt),
	}
}

func bestEffortWindowKindFromProto(k v1.BestEffortWindowKind) domain.BestEffortWindowKind {
	switch k {
	case v1.BestEffortWindowKind_BEST_EFFORT_WINDOW_KIND_DISTANCE:
		return domain.BestEffortWindowDistance
	case v1.BestEffortWindowKind_BEST_EFFORT_WINDOW_KIND_DURATION:
		return domain.BestEffortWindowDuration
	}
	return ""
}
