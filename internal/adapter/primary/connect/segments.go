package connect

import (
	"context"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// SegmentServer implements the caller-centric reads of
// cairnv1connect.SegmentServiceHandler (ListMySegments, GetSegment,
// ListMySegmentEfforts); the rest stays Unimplemented until backed.
type SegmentServer struct {
	cairnv1connect.UnimplementedSegmentServiceHandler

	Segments *view.Segments
}

var _ cairnv1connect.SegmentServiceHandler = (*SegmentServer)(nil)

// NewSegmentServer wires the server.
func NewSegmentServer(segments *view.Segments) *SegmentServer {
	return &SegmentServer{Segments: segments}
}

func (s *SegmentServer) segments() (*view.Segments, error) {
	if s.Segments == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "segments are not available on this instance")
	}
	return s.Segments, nil
}

func parseSegmentID(raw string) (domain.SegmentID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return domain.SegmentID{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid segment id")
	}
	return domain.SegmentID(id), nil
}

func (s *SegmentServer) ListMySegments(ctx context.Context, req *v1.ListMySegmentsRequest) (*v1.ListMySegmentsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	segs, err := s.segments()
	if err != nil {
		return nil, err
	}
	activityType := ""
	if t, ok := activityTypeFromProto(req.GetActivityType()); ok {
		activityType = string(t)
	}
	limit, offset := pageFrom(req.GetPage())
	page, err := segs.Mine(ctx, userID, activityType, limit, offset)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListMySegmentsResponse{Page: &v1.PageResponse{Total: -1}}
	if page.Stats != nil {
		st := page.Stats
		out.Stats = &v1.UserSegmentStats{Segments: connectClampInt32(st.Segments), Efforts: connectClampInt32(st.Efforts), Prs: connectClampInt32(st.PRs),
			Crs: connectClampInt32(st.CRs), External: connectClampInt32(st.External), Native: connectClampInt32(st.Native)}
	}
	for _, it := range page.Items {
		out.Segments = append(out.Segments, &v1.UserSegmentRow{
			Id: it.ID.String(), Name: it.Name, ActivityType: activityTypeToProto(domain.ActivityType(it.ActivityType)), Source: string(it.Source),
			DistanceM: it.DistanceM, ElevationGainM: it.ElevationGainM, AvgGrade: it.AvgGrade, EffortCount: connectClampInt32(it.EffortCount),
			BestElapsedS: it.BestElapsedS, LastEffortAt: timestamppb.New(it.LastEffortAt), HasPr: it.HasPR, HasCr: it.HasCR,
		})
	}
	if page.HasMore {
		out.Page.NextCursor = encodeOffset(offset + len(page.Items))
	}
	return out, nil
}

// GetSegment returns a segment the caller may see (has efforts on, or owns)
// with their own efforts as top_efforts when asked.
func (s *SegmentServer) GetSegment(ctx context.Context, req *v1.GetSegmentRequest) (*v1.GetSegmentResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	segs, err := s.segments()
	if err != nil {
		return nil, err
	}
	id, err := parseSegmentID(req.GetId())
	if err != nil {
		return nil, err
	}
	seg, efforts, err := segs.Detail(ctx, userID, id)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetSegmentResponse{Segment: segmentToProto(seg)}
	if req.GetIncludeTopEfforts() {
		for _, e := range efforts {
			out.TopEfforts = append(out.TopEfforts, segmentEffortToProto(e))
		}
	}
	return out, nil
}

func (s *SegmentServer) ListMySegmentEfforts(ctx context.Context, req *v1.ListMySegmentEffortsRequest) (*v1.ListMySegmentEffortsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	segs, err := s.segments()
	if err != nil {
		return nil, err
	}
	id, err := parseSegmentID(req.GetSegmentId())
	if err != nil {
		return nil, err
	}
	limit, offset := pageFrom(req.GetPage())
	efforts, err := segs.MyEfforts(ctx, userID, id, limit, offset)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListMySegmentEffortsResponse{Page: &v1.PageResponse{Total: -1}}
	for _, e := range efforts {
		if !effortInRange(e, req.GetTimeRange()) {
			continue
		}
		wire := segmentEffortToProto(e)
		out.Efforts = append(out.Efforts, wire)
		if e.IsPersonalRecord && out.PersonalRecord == nil {
			out.PersonalRecord = wire
		}
	}
	if len(efforts) == limit {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

// effortInRange applies an optional [from, to) filter on the effort start.
func effortInRange(e domain.SegmentEffort, tr *v1.TimeRange) bool {
	if tr == nil {
		return true
	}
	if from := tr.GetFrom(); from != nil && e.StartTime.Before(from.AsTime()) {
		return false
	}
	if to := tr.GetTo(); to != nil && !e.StartTime.Before(to.AsTime()) {
		return false
	}
	return true
}

func segmentToProto(seg domain.Segment) *v1.Segment {
	out := &v1.Segment{
		Id:                   seg.ID.String(),
		Source:               string(seg.Source),
		ExternalId:           seg.ExternalID,
		Scope:                segmentScopeToProto(seg.Scope),
		Name:                 seg.Name,
		Description:          seg.Description,
		ActivityType:         activityTypeToProto(seg.ActivityType),
		Polyline:             &v1.EncodedPolyline{Encoded: seg.Polyline, Precision: connectClampInt32(seg.PolylinePrecision)},
		DistanceM:            seg.DistanceM,
		ElevationGainM:       seg.ElevationGainM,
		ElevationLossM:       seg.ElevationLossM,
		MinElevationM:        seg.MinElevationM,
		MaxElevationM:        seg.MaxElevationM,
		AvgGrade:             seg.AvgGrade,
		MaxGrade:             seg.MaxGrade,
		MatchCorridorM:       seg.MatchCorridorM,
		MatchStartToleranceM: seg.MatchStartToleranceM,
		MatchEndToleranceM:   seg.MatchEndToleranceM,
		Bidirectional:        seg.Bidirectional,
		Starred:              seg.Starred,
		CreatedAt:            timestamppb.New(seg.CreatedAt),
		UpdatedAt:            timestamppb.New(seg.UpdatedAt),
	}
	if seg.ExternalAccountID != nil {
		v := seg.ExternalAccountID.String()
		out.ExternalAccountId = &v
	}
	if seg.OwnerUserID != nil {
		v := seg.OwnerUserID.String()
		out.OwnerUserId = &v
	}
	if c := climbCategoryToProto(seg.ClimbCategory); c != v1.ClimbCategory_CLIMB_CATEGORY_UNSPECIFIED {
		out.ClimbCategory = &c
	}
	return out
}

func segmentScopeToProto(s domain.SegmentScope) v1.SegmentScope {
	switch s {
	case domain.SegmentScopePrivate:
		return v1.SegmentScope_SEGMENT_SCOPE_PRIVATE
	case domain.SegmentScopeInstance:
		return v1.SegmentScope_SEGMENT_SCOPE_INSTANCE
	}
	return v1.SegmentScope_SEGMENT_SCOPE_UNSPECIFIED
}

func climbCategoryToProto(c domain.ClimbCategory) v1.ClimbCategory {
	switch c {
	case domain.ClimbCategoryNone:
		return v1.ClimbCategory_CLIMB_CATEGORY_NONE
	case domain.ClimbCategory4:
		return v1.ClimbCategory_CLIMB_CATEGORY_FOUR
	case domain.ClimbCategory3:
		return v1.ClimbCategory_CLIMB_CATEGORY_THREE
	case domain.ClimbCategory2:
		return v1.ClimbCategory_CLIMB_CATEGORY_TWO
	case domain.ClimbCategory1:
		return v1.ClimbCategory_CLIMB_CATEGORY_ONE
	case domain.ClimbCategoryHorsCategorie:
		return v1.ClimbCategory_CLIMB_CATEGORY_HORS_CATEGORIE
	}
	return v1.ClimbCategory_CLIMB_CATEGORY_UNSPECIFIED
}

func segmentEffortToProto(e domain.SegmentEffort) *v1.SegmentEffort {
	return &v1.SegmentEffort{
		Id:                       e.ID.String(),
		SegmentId:                e.SegmentID.String(),
		ActivityId:               e.ActivityID.String(),
		ActivitySourceId:         e.ActivitySourceID.String(),
		UserId:                   e.UserID.String(),
		StartTime:                timestamppb.New(e.StartTime),
		Elapsed:                  durationpb.New(secondsDuration(e.ElapsedS)),
		Moving:                   durationpb.New(secondsDuration(e.MovingS)),
		StartOffset:              connectClampInt32(e.StartOffset),
		EndOffset:                connectClampInt32(e.EndOffset),
		AvgHeartRateBpm:          intPtrToInt32(e.AvgHeartRateBpm),
		MaxHeartRateBpm:          intPtrToInt32(e.MaxHeartRateBpm),
		AvgPowerW:                intPtrToInt32(e.AvgPowerW),
		AvgCadence:               intPtrToInt32(e.AvgCadence),
		AvgSpeedMps:              e.AvgSpeedMps,
		PersonalRank:             connectClampInt32(e.PersonalRank),
		InstanceRank:             connectClampInt32(e.InstanceRank),
		IsPersonalRecord:         e.IsPersonalRecord,
		IsInstanceRecord:         e.IsInstanceRecord,
		ProviderEffortExternalId: e.ProviderEffortExternalID,
		CreatedAt:                timestamppb.New(e.CreatedAt),
	}
}
