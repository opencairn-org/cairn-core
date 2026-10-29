package connect

import (
	"context"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Native authoring, browsing, leaderboards and rematching. Authoring is nil
// when matching is not wired; RematchSegment (optional) runs the backfill
// in the background and returns a job id.

func (s *SegmentServer) authoring() error {
	if s.Authoring == nil {
		return unavailable("segment authoring")
	}
	return nil
}

func (s *SegmentServer) ListSegments(ctx context.Context, req *v1.ListSegmentsRequest) (*v1.ListSegmentsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	segs, err := s.segments()
	if err != nil {
		return nil, err
	}
	limit, offset := pageFrom(req.GetPage())
	f := domain.SegmentListFilter{
		OnlyWithEfforts: req.GetOnlyWithEfforts(), OnlyStarred: req.GetOnlyStarred(), Query: req.GetQuery(),
		Sort: segmentSortFromProto(req.GetSort()), Limit: limit, Offset: offset,
	}
	for _, src := range req.GetSources() {
		f.Sources = append(f.Sources, segmentSourceFromWire(src))
	}
	for _, sc := range req.GetScopes() {
		if v, ok := segmentScopeFromProto(sc); ok {
			f.Scopes = append(f.Scopes, v)
		}
	}
	for _, at := range req.GetActivityTypes() {
		if v, ok := activityTypeFromProto(at); ok {
			f.ActivityTypes = append(f.ActivityTypes, v)
		}
	}
	list, total, err := segs.List(ctx, userID, f)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListSegmentsResponse{Page: &v1.PageResponse{Total: int64(total)}}
	for _, seg := range list {
		out.Segments = append(out.Segments, segmentToProto(seg))
	}
	if offset+len(list) < total {
		out.Page.NextCursor = encodeOffset(offset + len(list))
	}
	return out, nil
}

func (s *SegmentServer) SearchSegmentsInBoundingBox(ctx context.Context, req *v1.SearchSegmentsInBoundingBoxRequest) (*v1.SearchSegmentsInBoundingBoxResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	segs, err := s.segments()
	if err != nil {
		return nil, err
	}
	if req.GetBbox() == nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bbox is required")
	}
	b := req.GetBbox()
	box := port.BoundingBox{MinLat: b.GetMinLatitude(), MaxLat: b.GetMaxLatitude(), MinLon: b.GetMinLongitude(), MaxLon: b.GetMaxLongitude()}
	var types []domain.ActivityType
	for _, at := range req.GetActivityTypes() {
		if v, ok := activityTypeFromProto(at); ok {
			types = append(types, v)
		}
	}
	list, err := segs.SearchBBox(ctx, userID, box, types, req.GetIncludeInstanceSegments(), int(req.GetLimit()))
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.SearchSegmentsInBoundingBoxResponse{}
	for _, seg := range list {
		out.Segments = append(out.Segments, segmentToProto(seg))
	}
	return out, nil
}

func (s *SegmentServer) ListSegmentEfforts(ctx context.Context, req *v1.ListSegmentEffortsRequest) (*v1.ListSegmentEffortsResponse, error) {
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
	q := domain.SegmentEffortQuery{Limit: limit, Offset: offset, FastestFirst: req.GetSort() == v1.SegmentEffortSort_SEGMENT_EFFORT_SORT_ELAPSED_ASC}
	q.From, q.To = timeRangeBounds(req.GetTimeRange())
	if req.UserId != nil {
		uid, err := uuid.Parse(req.GetUserId())
		if err != nil {
			return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad user_id")
		}
		u := domain.UserID(uid)
		q.UserID = &u
	}
	efforts, err := segs.Efforts(ctx, userID, id, q)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListSegmentEffortsResponse{Page: &v1.PageResponse{Total: -1}}
	for _, e := range efforts {
		out.Efforts = append(out.Efforts, segmentEffortToProto(e))
	}
	if len(efforts) == limit {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

func (s *SegmentServer) GetSegmentLeaderboard(ctx context.Context, req *v1.GetSegmentLeaderboardRequest) (*v1.GetSegmentLeaderboardResponse, error) {
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
	if req.BiologicalSex != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "biological_sex filter is not supported: profiles do not share it")
	}
	limit, offset := pageFrom(req.GetPage())
	q := domain.LeaderboardQuery{Limit: limit, Offset: offset}
	q.From, q.To = timeRangeBounds(req.GetTimeRange())
	entries, mine, err := segs.Leaderboard(ctx, userID, id, q)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.GetSegmentLeaderboardResponse{Page: &v1.PageResponse{Total: -1}}
	for _, e := range entries {
		out.Entries = append(out.Entries, leaderboardEntryToProto(e))
	}
	if mine != nil {
		rank := connectClampInt32(mine.Rank)
		out.MyRank, out.MyEntry = &rank, leaderboardEntryToProto(*mine)
	}
	if len(entries) == limit {
		out.Page.NextCursor = encodeOffset(offset + limit)
	}
	return out, nil
}

func (s *SegmentServer) CreateSegment(ctx context.Context, req *v1.CreateSegmentRequest) (*v1.CreateSegmentResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authoring(); err != nil {
		return nil, err
	}
	at, ok := activityTypeFromProto(req.GetActivityType())
	if !ok {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "activity_type is required")
	}
	scope, _ := segmentScopeFromProto(req.GetScope())
	seg, err := s.Authoring.Create(ctx, userID, domain.NativeSegmentInput{
		Name: req.GetName(), Description: req.GetDescription(), ActivityType: at,
		Polyline: req.GetPolyline().GetEncoded(), PolylinePrecision: int(req.GetPolyline().GetPrecision()), Scope: scope,
		MatchCorridorM: req.MatchCorridorM, MatchStartToleranceM: req.MatchStartToleranceM, MatchEndToleranceM: req.MatchEndToleranceM,
		Bidirectional: req.GetBidirectional(),
	})
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.CreateSegmentResponse{Segment: segmentToProto(seg)}
	if req.GetBackfillEfforts() && s.Backfill != nil {
		job := s.Backfill(userID, seg.ID, time.Time{}, time.Time{})
		out.BackfillJobId = &job
	}
	return out, nil
}

func (s *SegmentServer) UpdateSegment(ctx context.Context, req *v1.UpdateSegmentRequest) (*v1.UpdateSegmentResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authoring(); err != nil {
		return nil, err
	}
	id, err := parseSegmentID(req.GetId())
	if err != nil {
		return nil, err
	}
	patch, err := segmentPatchFromProto(req)
	if err != nil {
		return nil, err
	}
	seg, err := s.Authoring.Update(ctx, userID, id, patch)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.UpdateSegmentResponse{Segment: segmentToProto(seg)}, nil
}

// segmentPatchFromProto applies field_mask (empty = every set field).
func segmentPatchFromProto(req *v1.UpdateSegmentRequest) (domain.SegmentPatch, error) {
	mask, err := segmentMask(req.GetFieldMask())
	if err != nil {
		return domain.SegmentPatch{}, err
	}
	in := func(f string) bool { return len(mask) == 0 || mask[f] }
	p := domain.SegmentPatch{}
	if in("name") {
		p.Name = req.Name
	}
	if in("description") {
		p.Description = req.Description
	}
	if in("scope") && req.Scope != nil {
		if sc, ok := segmentScopeFromProto(req.GetScope()); ok {
			p.Scope = &sc
		}
	}
	if in("match_corridor_m") {
		p.MatchCorridorM = req.MatchCorridorM
	}
	if in("match_start_tolerance_m") {
		p.MatchStartToleranceM = req.MatchStartToleranceM
	}
	if in("match_end_tolerance_m") {
		p.MatchEndToleranceM = req.MatchEndToleranceM
	}
	if in("bidirectional") {
		p.Bidirectional = req.Bidirectional
	}
	if in("starred") {
		p.Starred = req.Starred
	}
	if in("polyline") && req.Polyline != nil {
		enc := req.GetPolyline().GetEncoded()
		p.Polyline, p.PolylinePrecision = &enc, int(req.GetPolyline().GetPrecision())
	}
	return p, nil
}

// segmentMask validates the field_mask entries.
func segmentMask(fields []string) (map[string]bool, error) {
	mask := map[string]bool{}
	for _, f := range fields {
		switch f {
		case "name", "description", "scope", "match_corridor_m", "match_start_tolerance_m", "match_end_tolerance_m", "bidirectional", "starred", "polyline":
			mask[f] = true
		default:
			return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "unknown field_mask entry "+f)
		}
	}
	return mask, nil
}

func (s *SegmentServer) DeleteSegment(ctx context.Context, req *v1.DeleteSegmentRequest) (*v1.DeleteSegmentResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authoring(); err != nil {
		return nil, err
	}
	id, err := parseSegmentID(req.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.Authoring.Delete(ctx, userID, id, req.GetKeepEfforts()); err != nil {
		return nil, actionErr(err)
	}
	return &v1.DeleteSegmentResponse{}, nil
}

func (s *SegmentServer) RematchActivity(ctx context.Context, req *v1.RematchActivityRequest) (*v1.RematchActivityResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.authoring(); err != nil {
		return nil, err
	}
	aid, err := uuid.Parse(req.GetActivityId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "bad activity id")
	}
	if _, _, err := s.Authoring.RematchActivity(ctx, userID, domain.ActivityID(aid)); err != nil {
		return nil, actionErr(err)
	}
	return &v1.RematchActivityResponse{JobId: "rematch-activity:" + aid.String()}, nil
}

func (s *SegmentServer) RematchSegment(ctx context.Context, req *v1.RematchSegmentRequest) (*v1.RematchSegmentResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	segs, err := s.segments()
	if err != nil {
		return nil, err
	}
	if s.Backfill == nil {
		return nil, unavailable("segment matching")
	}
	id, err := parseSegmentID(req.GetSegmentId())
	if err != nil {
		return nil, err
	}
	if _, err := segs.Visible(ctx, userID, id); err != nil {
		return nil, actionErr(err)
	}
	from, to := timeRangeBounds(req.GetTimeRange())
	return &v1.RematchSegmentResponse{JobId: s.Backfill(userID, id, from, to)}, nil
}

// ---- converters ----

func leaderboardEntryToProto(e domain.LeaderboardEntry) *v1.LeaderboardEntry {
	out := &v1.LeaderboardEntry{
		Rank: connectClampInt32(e.Rank), UserId: e.UserID.String(), UserDisplayName: e.DisplayName,
		Elapsed: durationpb.New(secondsDuration(e.ElapsedS)), StartTime: timestamppb.New(e.StartTime),
		EffortId: e.EffortID.String(), ActivityId: e.ActivityID.String(),
	}
	if e.AvatarURL != "" {
		a := e.AvatarURL
		out.UserAvatarUrl = &a
	}
	return out
}

func segmentScopeFromProto(s v1.SegmentScope) (domain.SegmentScope, bool) {
	switch s {
	case v1.SegmentScope_SEGMENT_SCOPE_PRIVATE:
		return domain.SegmentScopePrivate, true
	case v1.SegmentScope_SEGMENT_SCOPE_INSTANCE:
		return domain.SegmentScopeInstance, true
	}
	return "", false
}

// segmentSourceFromWire accepts the stored names and the proto's historic
// "strava"/"cairn" spellings.
func segmentSourceFromWire(s string) domain.SegmentSource {
	switch s {
	case "cairn", "native":
		return domain.SegmentSourceNative
	case "strava", "external":
		return domain.SegmentSourceExternal
	}
	return domain.SegmentSource(s)
}

func segmentSortFromProto(s v1.SegmentSort) domain.SegmentSort {
	switch s {
	case v1.SegmentSort_SEGMENT_SORT_DISTANCE_DESC:
		return domain.SegmentSortDistanceDesc
	case v1.SegmentSort_SEGMENT_SORT_ELEVATION_DESC:
		return domain.SegmentSortElevationDesc
	case v1.SegmentSort_SEGMENT_SORT_EFFORT_COUNT_DESC:
		return domain.SegmentSortEffortCountDesc
	case v1.SegmentSort_SEGMENT_SORT_CREATED_AT_DESC:
		return domain.SegmentSortCreatedAtDesc
	}
	return domain.SegmentSortNameAsc
}

// timeRangeBounds converts an optional [from, to) range; zero = unbounded.
func timeRangeBounds(tr *v1.TimeRange) (from, to time.Time) {
	if tr == nil {
		return from, to
	}
	if tr.GetFrom() != nil {
		from = tr.GetFrom().AsTime()
	}
	if tr.GetTo() != nil {
		to = tr.GetTo().AsTime()
	}
	return from, to
}
