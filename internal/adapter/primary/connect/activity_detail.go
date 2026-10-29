package connect

import (
	"context"
	"fmt"
	"math"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/streamalign"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// ---------------------------------------------------------------------------
// Per-activity detail reads (owner only; kudos/comments gated on the social
// category). All rendered from usecase/view.Details, like the REST routes.
// ---------------------------------------------------------------------------

func (s *ActivityServer) details() (*view.Details, error) {
	if s.Details == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "activity details are not available on this instance")
	}
	return s.Details, nil
}

func (s *ActivityServer) ListLaps(ctx context.Context, req *v1.ListLapsRequest) (*v1.ListLapsResponse, error) {
	d, err := s.details()
	if err != nil {
		return nil, err
	}
	act, err := s.activityUpdateLoad(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	laps, sid, err := d.Laps(ctx, act)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListLapsResponse{Laps: make([]*v1.ActivityLap, 0, len(laps))}
	if sid != nil {
		out.SourceId = sid.String()
	}
	for i, l := range laps {
		out.Laps = append(out.Laps, lapToProto(l, i))
	}
	return out, nil
}

func lapToProto(l domain.ActivityLap, pos int) *v1.ActivityLap {
	idx := l.Index
	if idx == 0 {
		idx = pos + 1
	}
	return &v1.ActivityLap{
		Index:           connectClampInt32(idx),
		ElapsedDuration: durationpb.New(l.ElapsedDuration),
		MovingDuration:  durationpb.New(l.MovingDuration),
		DistanceM:       l.DistanceM,
		AvgHeartRateBpm: intPtrToInt32(l.AvgHeartRateBpm),
		AvgPowerW:       intPtrToInt32(l.AvgPowerW),
		AvgSpeedMps:     l.AvgSpeedMps,
		AvgCadence:      intPtrToInt32(l.AvgCadence),
		ElevationGainM:  l.ElevationGainM,
		ElevationLossM:  l.ElevationLossM,
		Label:           l.Label,
		StartOffsetS:    int64(l.StartOffset.Seconds()),
	}
}

func (s *ActivityServer) GetActivityZones(ctx context.Context, req *v1.GetActivityZonesRequest) (*v1.GetActivityZonesResponse, error) {
	d, err := s.details()
	if err != nil {
		return nil, err
	}
	act, err := s.activityUpdateLoad(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	z, err := d.Zones(ctx, act)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetActivityZonesResponse{Hr: zoneSummaryToProto(z.HR), Power: zoneSummaryToProto(z.Power)}
	if z.Decoupling != nil {
		out.Decoupling = &v1.Decoupling{Pct: z.Decoupling.Pct, Basis: z.Decoupling.Basis, Coupled: z.Decoupling.Coupled}
	}
	return out, nil
}

func zoneSummaryToProto(z *domain.ZoneSummary) *v1.ZoneSummary {
	if z == nil {
		return nil
	}
	out := &v1.ZoneSummary{Basis: z.Basis, Reference: z.Reference, TotalSeconds: connectClampInt32(z.TotalSeconds)}
	for _, b := range z.Zones {
		out.Zones = append(out.Zones, &v1.ZoneBand{Label: b.Label, LowPct: b.LowPct, HighPct: b.HighPct, Seconds: connectClampInt32(b.Seconds)})
	}
	return out
}

func (s *ActivityServer) ListActivitySegmentEfforts(ctx context.Context, req *v1.ListActivitySegmentEffortsRequest) (*v1.ListActivitySegmentEffortsResponse, error) {
	d, err := s.details()
	if err != nil {
		return nil, err
	}
	act, err := s.activityUpdateLoad(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	rows, err := d.SegmentEfforts(ctx, act)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListActivitySegmentEffortsResponse{Efforts: make([]*v1.ActivitySegmentEffort, 0, len(rows))}
	for _, row := range rows {
		out.Efforts = append(out.Efforts, effortRowToProto(row))
	}
	return out, nil
}

func effortRowToProto(row view.EffortRow) *v1.ActivitySegmentEffort {
	e, seg := row.Effort, row.Segment
	out := &v1.ActivitySegmentEffort{
		SegmentId:        e.SegmentID.String(),
		SegmentName:      seg.Name,
		SegmentDistanceM: seg.DistanceM,
		ElapsedS:         e.ElapsedS,
		MovingS:          e.MovingS,
		AvgHeartRate:     intPtrToInt32(e.AvgHeartRateBpm),
		AvgPower:         intPtrToInt32(e.AvgPowerW),
		IsPersonalRecord: e.IsPersonalRecord,
		PersonalRank:     connectClampInt32(e.PersonalRank),
		StartOffsetS:     row.StartOffsetS,
		EndOffsetS:       row.EndOffsetS,
		StartDistanceM:   row.StartDistanceM,
		EndDistanceM:     row.EndDistanceM,
	}
	if seg.ClimbCategory != "" && seg.ClimbCategory != domain.ClimbCategoryNone {
		c := string(seg.ClimbCategory)
		out.ClimbCategory = &c
	}
	return out
}

func (s *ActivityServer) ListSimilarActivities(ctx context.Context, req *v1.ListSimilarActivitiesRequest) (*v1.ListSimilarActivitiesResponse, error) {
	d, err := s.details()
	if err != nil {
		return nil, err
	}
	act, err := s.activityUpdateLoad(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	g, err := d.Similar(ctx, act)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListSimilarActivitiesResponse{
		TargetSegments: connectClampInt32(g.TargetSegments),
		Count:          connectClampInt32(len(g.Rows)),
		BestMovingS:    g.BestMovingS,
		CurrentMovingS: g.CurrentMovingS,
		CurrentRank:    connectClampInt32(g.CurrentRank),
		IsBest:         g.IsBest,
	}
	for _, row := range g.Rows {
		out.Activities = append(out.Activities, &v1.SimilarActivity{
			Id:             row.ActivityID.String(),
			IsCurrent:      row.IsCurrent,
			Title:          row.Title,
			Type:           activityTypeToProto(domain.ActivityType(row.Type)),
			StartTime:      timestamppb.New(row.StartTime),
			DistanceM:      row.DistanceM,
			MovingS:        row.MovingS,
			ElapsedS:       row.ElapsedS,
			AvgHeartRate:   intPtrToInt32(row.AvgHeartRateBpm),
			AvgPower:       intPtrToInt32(row.AvgPowerW),
			PaceSPerKm:     row.PaceSPerKm,
			SharedSegments: connectClampInt32(row.SharedSegments),
			OverlapPct:     row.OverlapPct,
		})
	}
	return out, nil
}

func (s *ActivityServer) GetMergedStream(ctx context.Context, req *v1.GetMergedStreamRequest) (*v1.GetMergedStreamResponse, error) {
	d, err := s.details()
	if err != nil {
		return nil, err
	}
	act, err := s.activityUpdateLoad(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	merged, has, err := d.MergedStream(ctx, act)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetMergedStreamResponse{Channels: map[string]*v1.MergedChannel{}}
	if !has {
		return out, nil
	}
	return mergedStreamToProto(merged), nil
}

func mergedStreamToProto(m streamalign.MergedStream) *v1.GetMergedStreamResponse {
	out := &v1.GetMergedStreamResponse{GridUnixS: make([]int64, len(m.Grid)), Channels: map[string]*v1.MergedChannel{}}
	for i, t := range m.Grid {
		out.GridUnixS[i] = t.Unix()
	}
	for ch, vals := range m.Channels {
		mc := &v1.MergedChannel{Values: make([]float64, len(vals))}
		for i, v := range vals {
			if v == nil {
				mc.Values[i] = math.NaN()
			} else {
				mc.Values[i] = *v
			}
		}
		if sid, ok := m.Provenance[ch]; ok {
			mc.SourceId = sid.String()
		}
		out.Channels[string(ch)] = mc
	}
	return out
}

// socialActivity loads the activity and checks the caller may see reactions
// on it: the owner, or a viewer holding the social category.
func (s *ActivityServer) socialActivity(ctx context.Context, rawID string) (domain.Activity, domain.UserID, *view.Details, error) {
	d, err := s.details()
	if err != nil {
		return domain.Activity{}, domain.UserID{}, nil, err
	}
	me, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return domain.Activity{}, domain.UserID{}, nil, err
	}
	id, err := parseActivityID(rawID)
	if err != nil {
		return domain.Activity{}, domain.UserID{}, nil, err
	}
	act, err := s.Activities.GetActivity(ctx, id)
	if err != nil || act.IsDeleted() {
		return domain.Activity{}, domain.UserID{}, nil, connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	}
	if !d.SocialAllowed(ctx, act, me) {
		return domain.Activity{}, domain.UserID{}, nil, connectrpc.NewError(connectrpc.CodePermissionDenied, "not permitted")
	}
	return act, me, d, nil
}

func (s *ActivityServer) ListKudos(ctx context.Context, req *v1.ListKudosRequest) (*v1.ListKudosResponse, error) {
	act, me, d, err := s.socialActivity(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	v, err := d.Kudos(ctx, act, me)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListKudosResponse{Count: connectClampInt32(v.Count), HasKudos: v.HasKudos}
	for _, c := range v.Kudosers {
		out.Kudosers = append(out.Kudosers, engagementCardToProto(c))
	}
	return out, nil
}

func (s *ActivityServer) ListComments(ctx context.Context, req *v1.ListCommentsRequest) (*v1.ListCommentsResponse, error) {
	act, _, d, err := s.socialActivity(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	comments, err := d.Comments(ctx, act)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListCommentsResponse{Comments: make([]*v1.Comment, 0, len(comments))}
	for _, c := range comments {
		out.Comments = append(out.Comments, &v1.Comment{Id: c.ID, Body: c.Body, CreatedAt: timestamppb.New(c.CreatedAt), Author: engagementCardToProto(c.Author)})
	}
	return out, nil
}

func engagementCardToProto(c view.EngagementCard) *v1.UserCard {
	if c.Remote {
		return &v1.UserCard{Username: c.Handle, DisplayName: c.Handle, Remote: true, Actor: c.ActorID}
	}
	return userCardToProto(c.User)
}

func (s *ActivityServer) GetActivityManageInfo(ctx context.Context, req *v1.GetActivityManageInfoRequest) (*v1.GetActivityManageInfoResponse, error) {
	d, err := s.details()
	if err != nil {
		return nil, err
	}
	act, err := s.activityUpdateLoad(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	info, err := d.Manage(ctx, act)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("manage info: %w", err))
	}
	out := &v1.GetActivityManageInfoResponse{MergedAt: timestamppb.New(act.MergedAt)}
	if info.Primary != nil {
		out.PrimarySourceId = info.Primary.String()
	}
	for _, in := range info.Sources {
		out.Sources = append(out.Sources, sourceInsightToProto(in))
	}
	for _, p := range info.Provenance {
		out.Provenance = append(out.Provenance, &v1.FieldProvenance{
			Field: string(p.Field), FieldLabel: p.FieldLabel, SourceId: p.SourceID.String(), Provider: p.Provider, DecidedBy: p.DecidedBy,
			SyncedAt: timestamppb.New(act.MergedAt),
		})
	}
	return out, nil
}

func sourceInsightToProto(in domain.SourceInsight) *v1.SourceInsight {
	s := in.Source
	out := &v1.SourceInsight{
		Source:          sourceToProto(s),
		WorkerRelease:   s.SourceWorkerRelease,
		ParserVersion:   in.ParserVersion,
		DatasetVersion:  in.DatasetVersion,
		ReparseWith:     in.ReparseWith,
		Upgrade:         in.Upgrade,
		WorkerPackage:   s.SourceWorkerPackage,
		ReparseEligible: in.ReparseEligible,
		HasBlob:         s.RawBlobID != "",
		RawContentType:  s.RawContentType,
		RawSizeBytes:    s.RawSizeBytes,
		HasStream:       s.Parsed.HasStream,
		LapCount:        connectClampInt32(len(s.Parsed.Laps)),
		StatusReason:    s.StatusReason,
		ReimportReason:  s.ReimportStatusReason,
		IsPrimary:       in.IsPrimary,
		WonFieldGroups:  in.WonFieldGroups,
	}
	if s.LastReimportedAt != nil {
		out.LastReimportedAt = timestamppb.New(*s.LastReimportedAt)
	}
	return out
}

func (s *ActivityServer) GetParsedSource(ctx context.Context, req *v1.GetParsedSourceRequest) (*v1.GetParsedSourceResponse, error) {
	d, err := s.details()
	if err != nil {
		return nil, err
	}
	act, err := s.activityUpdateLoad(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	sid, err := parseSourceID(req.GetSourceId())
	if err != nil {
		return nil, err
	}
	src, err := d.ParsedSource(ctx, act, sid)
	if err != nil {
		return nil, notFoundErr(err)
	}
	return &v1.GetParsedSourceResponse{Payload: payloadToProto(src.Parsed)}, nil
}

// payloadToProto renders a stored source payload (the inverse of
// protoconv.ActivitySourcePayloadFromProto for the fields core keeps).
func payloadToProto(p domain.ActivitySourcePayload) *v1.ActivitySourcePayload {
	out := &v1.ActivitySourcePayload{
		Type:            activityTypeToProto(p.Type),
		Discipline:      disciplineToProto(p.Discipline),
		IsVirtual:       p.IsVirtual,
		IsEbike:         p.IsEbike,
		IsCommute:       p.IsCommute,
		IsRace:          p.IsRace,
		CustomSubtype:   p.CustomSubtype,
		Title:           p.Title,
		Description:     p.Description,
		StartTime:       timestamppb.New(p.StartTime),
		EndTime:         timestamppb.New(p.EndTime),
		ElapsedDuration: durationpb.New(p.ElapsedDuration),
		MovingDuration:  durationpb.New(p.MovingDuration),
		Timezone:        p.Timezone,
		Summary:         summaryToProto(p.Summary),
	}
	for i, l := range p.Laps {
		out.Laps = append(out.Laps, lapToProto(l, i))
	}
	if p.HasStream {
		out.StreamSummary = &v1.StreamSummary{}
	}
	return out
}
