package connect

import (
	"context"
	"fmt"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/dashboard"
	gearuc "github.com/opencairn-org/cairn-core/internal/usecase/gear"
)

// StatsServer implements cairnv1connect.StatsServiceHandler: aggregates over
// the caller's own activities.
type StatsServer struct {
	cairnv1connect.UnimplementedStatsServiceHandler

	Overviews *dashboard.Overviews
	Stats     *dashboard.Stats
	Content   port.ContentSigner
	Gear      *gearuc.Service
	MapETag   func(domain.Activity) string
}

var _ cairnv1connect.StatsServiceHandler = (*StatsServer)(nil)

// NewStatsServer wires the server.
func NewStatsServer(overviews *dashboard.Overviews, content port.ContentSigner, gear *gearuc.Service, mapETag func(domain.Activity) string) *StatsServer {
	return &StatsServer{Overviews: overviews, Content: content, Gear: gear, MapETag: mapETag}
}

func (s *StatsServer) GetOverview(ctx context.Context, _ *v1.GetOverviewRequest) (*v1.GetOverviewResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Overviews == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "overview is not available on this instance")
	}
	ov, err := s.Overviews.Get(ctx, userID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("overview: %w", err))
	}
	out := &v1.GetOverviewResponse{
		Totals:        totalsToProto(ov.Totals),
		BySport:       facetsToProto(ov.BySport),
		ThisWeek:      totalsToProto(ov.ThisWeek),
		LastWeek:      totalsToProto(ov.LastWeek),
		ThisWeekRange: &v1.TimeRange{From: timestamppb.New(ov.ThisWeekFrom), To: timestamppb.New(ov.ThisWeekTo)},
		LastWeekRange: &v1.TimeRange{From: timestamppb.New(ov.LastWeekFrom), To: timestamppb.New(ov.LastWeekTo)},
		Recent:        make([]*v1.Activity, 0, len(ov.Recent)),
	}
	names := gearNamesOf(ctx, s.Gear, ov.Recent...)
	for _, a := range ov.Recent {
		out.Recent = append(out.Recent, decorateActivity(ctx, activityToProto(a, nil), a, names, s.Content, s.MapETag))
	}
	return out, nil
}

func totalsToProto(t port.ActivityTotalsResult) *v1.Totals {
	return &v1.Totals{Count: connectClampInt32(t.Count), DistanceM: t.DistanceM, MovingS: t.MovingS, ElevationGainM: t.ElevationGainM}
}

func facetsToProto(in []port.ActivityFacet) []*v1.Facet {
	out := make([]*v1.Facet, 0, len(in))
	for _, f := range in {
		out = append(out, &v1.Facet{Value: f.Value, Count: connectClampInt32(f.Count)})
	}
	return out
}
