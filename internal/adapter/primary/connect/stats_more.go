package connect

import (
	"context"
	"fmt"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/twpayne/go-polyline"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

func (s *StatsServer) stats() (*view.Stats, error) {
	if s.Stats == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "stats are not available on this instance")
	}
	return s.Stats, nil
}

func (s *StatsServer) GetStats(ctx context.Context, _ *v1.GetStatsRequest) (*v1.GetStatsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.stats()
	if err != nil {
		return nil, err
	}
	res, err := st.Get(ctx, userID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("stats: %w", err))
	}
	out := &v1.GetStatsResponse{Totals: totalsToProto(res.Totals), BySport: facetsToProto(res.BySport), ByDiscipline: facetsToProto(res.ByDiscipline)}
	for _, y := range res.ByYear {
		out.ByYear = append(out.ByYear, &v1.YearStat{Year: connectClampInt32(y.Year), Count: connectClampInt32(y.Count), DistanceM: y.DistanceM, MovingS: y.MovingS, ElevationGainM: y.ElevationGainM})
	}
	if a := res.LongestDistance; a != nil {
		out.LongestDistance = decorateActivity(ctx, activityToProto(*a, nil), *a, gearNamesOf(ctx, s.Gear, *a), s.Content, s.MapETag, false)
	}
	if a := res.LongestDuration; a != nil {
		out.LongestDuration = decorateActivity(ctx, activityToProto(*a, nil), *a, gearNamesOf(ctx, s.Gear, *a), s.Content, s.MapETag, false)
	}
	return out, nil
}

func (s *StatsServer) GetTrainingLoad(ctx context.Context, req *v1.GetTrainingLoadRequest) (*v1.GetTrainingLoadResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.stats()
	if err != nil {
		return nil, err
	}
	series, err := st.TrainingLoad(ctx, userID, int(req.GetDays()))
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetTrainingLoadResponse{Days: make([]*v1.TrainingLoadDay, 0, len(series))}
	for _, d := range series {
		out.Days = append(out.Days, &v1.TrainingLoadDay{Date: d.Date, Ctl: d.CTL, Atl: d.ATL, Tsb: d.TSB, Tss: d.TSS})
	}
	if n := len(series); n > 0 {
		out.CurrentCtl, out.CurrentAtl, out.CurrentTsb = series[n-1].CTL, series[n-1].ATL, series[n-1].TSB
	}
	return out, nil
}

func (s *StatsServer) GetBestEffortHistory(ctx context.Context, req *v1.GetBestEffortHistoryRequest) (*v1.GetBestEffortHistoryResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.stats()
	if err != nil {
		return nil, err
	}
	t, _ := activityTypeFromProto(req.GetType())
	metric, _ := bestEffortMetricFromProto(req.GetMetric())
	h, err := st.BestEffortHistory(ctx, userID, string(t), string(metric), windowKindKey(req.GetWindowKind()), int(req.GetWindowValue()))
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetBestEffortHistoryResponse{Best: h.Best, Count: connectClampInt32(len(h.Items))}
	for _, it := range h.Items {
		out.Items = append(out.Items, &v1.BestEffortHistoryItem{ActivityId: it.ActivityID.String(), Title: it.Title, StartTime: timestamppb.New(it.StartTime), AchievedValue: it.AchievedValue, IsBest: it.AchievedValue == h.Best})
	}
	return out, nil
}

func windowKindKey(k v1.BestEffortWindowKind) string {
	switch k {
	case v1.BestEffortWindowKind_BEST_EFFORT_WINDOW_KIND_DISTANCE:
		return "distance"
	case v1.BestEffortWindowKind_BEST_EFFORT_WINDOW_KIND_DURATION:
		return "duration"
	case v1.BestEffortWindowKind_BEST_EFFORT_WINDOW_KIND_UNSPECIFIED:
	}
	return ""
}

func (s *StatsServer) GetHeatmap(ctx context.Context, req *v1.GetHeatmapRequest) (*v1.GetHeatmapResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.stats()
	if err != nil {
		return nil, err
	}
	filter, err := activityFilterFromProto(req.GetFilter())
	if err != nil {
		return nil, err
	}
	res := domain.StreamResolution30s
	if req.GetResolution() == v1.GetHeatmapRequest_RESOLUTION_5S {
		res = domain.StreamResolution5s
	}
	hm, err := st.Heatmap(ctx, userID, filter, res)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetHeatmapResponse{
		Matched:   connectClampInt32(hm.Matched),
		WithTrack: connectClampInt32(len(hm.Tracks)),
		Truncated: hm.Truncated,
		Facets:    &v1.ActivityFacets{Types: facetsToProto(hm.Types), Disciplines: facetsToProto(hm.Disciplines)},
	}
	for _, y := range hm.Years {
		out.Facets.Years = append(out.Facets.Years, &v1.Facet{Value: fmt.Sprint(y.Year), Count: connectClampInt32(y.Count)})
	}
	for _, t := range hm.Tracks {
		out.Tracks = append(out.Tracks, &v1.HeatmapTrack{
			ActivityId: t.Activity.ID.String(),
			Type:       activityTypeToProto(t.Activity.Type),
			Discipline: string(t.Activity.Discipline),
			Polyline:   encodePolyline(t.Points),
		})
	}
	return out, nil
}

// encodePolyline encodes a track with the Google polyline algorithm at
// precision 5 (~1 m), the heatmap's fidelity.
func encodePolyline(pts []domain.GeoPoint) *v1.EncodedPolyline {
	coords := make([][]float64, len(pts))
	for i, p := range pts {
		coords[i] = []float64{p.Lat, p.Lon}
	}
	return &v1.EncodedPolyline{Encoded: string(polyline.EncodeCoords(coords)), Precision: 5}
}
