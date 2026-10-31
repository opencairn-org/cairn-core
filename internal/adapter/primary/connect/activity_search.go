package connect

import (
	"context"
	"fmt"
	"strconv"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/dashboard"
	gearuc "github.com/opencairn-org/cairn-core/internal/usecase/gear"
)

// SearchActivities is the faceted list (GET /api/activities/feed).
func (s *ActivityServer) SearchActivities(ctx context.Context, req *v1.SearchActivitiesRequest) (*v1.SearchActivitiesResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.Searches == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "search is not available on this instance")
	}
	filter, err := activityFilterFromProto(req.GetFilter())
	if err != nil {
		return nil, err
	}
	filter.Sort = activitySortKey(req.GetSort())
	filter.Limit, filter.Offset = pageFrom(req.GetPage())
	res, err := s.Searches.Search(ctx, userID, filter)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("search activities: %w", err))
	}
	out := &v1.SearchActivitiesResponse{
		Activities: make([]*v1.Activity, 0, len(res.Activities)),
		Facets:     &v1.ActivityFacets{Types: facetsToProto(res.Types), Disciplines: facetsToProto(res.Disciplines)},
		Total:      connectClampInt32(res.Total),
		Matched:    connectClampInt32(res.Matched),
		Page:       &v1.PageResponse{Total: int64(res.Matched)},
	}
	for _, y := range res.Years {
		out.Facets.Years = append(out.Facets.Years, &v1.Facet{Value: strconv.Itoa(y.Year), Count: connectClampInt32(y.Count)})
	}
	names := s.gearNames(ctx, res.Activities...)
	for _, a := range res.Activities {
		wire := decorateActivity(ctx, activityToProto(a, nil), a, names, s.Content, s.MapETag)
		wire.GearUnset = dashboard.GearUnset(a, res.GearPrompt)
		out.Activities = append(out.Activities, wire)
	}
	if filter.Offset+len(res.Activities) < res.Matched {
		out.Page.NextCursor = encodeOffset(filter.Offset + len(res.Activities))
	}
	return out, nil
}

// activityFilterFromProto maps the wire filter onto the repo filter.
func activityFilterFromProto(f *v1.ActivityFilter) (port.ActivityListFilter, error) {
	var out port.ActivityListFilter
	if f == nil {
		return out, nil
	}
	if err := filterDimensions(f, &out); err != nil {
		return out, err
	}
	gear, err := filterGearIDs(f.GetGearIds())
	if err != nil {
		return out, err
	}
	out.GearIDs = gear
	out.IsVirtual, out.IsEbike, out.IsCommute, out.IsRace = flagPtr(f.GetVirtual()), flagPtr(f.GetEbike()), flagPtr(f.GetCommute()), flagPtr(f.GetRace())
	out.DistanceMinM, out.DistanceMaxM = nonNegative(f.DistanceMinM), nonNegative(f.DistanceMaxM)
	out.DurationMinS, out.DurationMaxS = nonNegative(f.DurationMinS), nonNegative(f.DurationMaxS)
	out.ElevationMinM, out.ElevationMaxM = nonNegative(f.ElevationMinM), nonNegative(f.ElevationMaxM)
	out.AvgSpeedMinMps, out.AvgSpeedMaxMps = nonNegative(f.SpeedMinMps), nonNegative(f.SpeedMaxMps)
	out.AvgHRMinBpm, out.AvgHRMaxBpm = nonNegative(f.HrMinBpm), nonNegative(f.HrMaxBpm)
	out.AvgPowerMinW, out.AvgPowerMaxW = nonNegative(f.PowerMinW), nonNegative(f.PowerMaxW)
	return out, nil
}

// filterDimensions sets type, discipline and the time range.
func filterDimensions(f *v1.ActivityFilter, out *port.ActivityListFilter) error {
	if len(f.GetTypes()) > 1 || len(f.GetDisciplines()) > 1 {
		return connectrpc.NewError(connectrpc.CodeInvalidArgument, "only one type and one discipline can be filtered at a time")
	}
	if len(f.GetTypes()) == 1 {
		t, ok := activityTypeFromProto(f.GetTypes()[0])
		if !ok {
			return connectrpc.NewError(connectrpc.CodeInvalidArgument, "unknown activity type")
		}
		out.Type = string(t)
	}
	if len(f.GetDisciplines()) == 1 {
		out.Discipline = f.GetDisciplines()[0]
	}
	if tr := f.GetTimeRange(); tr != nil {
		if tr.GetFrom() != nil {
			out.From = tr.GetFrom().AsTime()
		}
		if tr.GetTo() != nil {
			out.To = tr.GetTo().AsTime()
		}
	}
	return nil
}

// filterGearIDs parses and bounds the gear filter.
func filterGearIDs(raw []string) ([]domain.GearID, error) {
	if len(raw) > feedGearIDsMax {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, fmt.Sprintf("at most %d gear_ids", feedGearIDsMax))
	}
	out := make([]domain.GearID, 0, len(raw))
	for _, r := range raw {
		id, err := uuid.Parse(r)
		if err != nil {
			return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid gear id "+r)
		}
		out = append(out, domain.GearID(id))
	}
	return out, nil
}

// feedGearIDsMax bounds the gear filter (same as the REST ?gear_id= cap).
const feedGearIDsMax = 50

func flagPtr(f v1.ActivityFilter_Flag) *bool {
	switch f {
	case v1.ActivityFilter_FLAG_ONLY:
		v := true
		return &v
	case v1.ActivityFilter_FLAG_EXCLUDE:
		v := false
		return &v
	case v1.ActivityFilter_FLAG_UNSPECIFIED:
	}
	return nil
}

// nonNegative drops a negative bound (the REST parser does the same).
func nonNegative(v *float64) *float64 {
	if v == nil || *v < 0 {
		return nil
	}
	return v
}

// activitySortKey maps the wire sort onto the repo's sort keys.
func activitySortKey(s v1.ActivitySort) string {
	switch s {
	case v1.ActivitySort_ACTIVITY_SORT_START_TIME_ASC:
		return "date_asc"
	case v1.ActivitySort_ACTIVITY_SORT_DISTANCE_DESC:
		return "distance_desc"
	case v1.ActivitySort_ACTIVITY_SORT_DURATION_DESC:
		return "duration_desc"
	case v1.ActivitySort_ACTIVITY_SORT_ELEVATION_DESC:
		return "elevation_desc"
	case v1.ActivitySort_ACTIVITY_SORT_SPEED_DESC:
		return "speed_desc"
	case v1.ActivitySort_ACTIVITY_SORT_UNSPECIFIED, v1.ActivitySort_ACTIVITY_SORT_START_TIME_DESC:
	}
	return "date_desc"
}

// decorateActivity adds the gear name and the signed course image to a wire
// activity. Shared by every server that returns Activity messages.
func decorateActivity(ctx context.Context, out *v1.Activity, a domain.Activity, names map[domain.GearID]string, content port.ContentSigner, etag func(domain.Activity) string) *v1.Activity {
	withGear(out, a, names)
	if content == nil || a.PrimaryStreamSourceID == nil || etag == nil {
		return out
	}
	if u, err := content.MapImage(ctx, a.ID, false); err == nil {
		out.MapImage = contentURLToProto(u)
		out.MapImageEtag = etag(a)
	}
	return out
}

// gearNamesOf resolves gear names for activities (one query); nil without gear.
func gearNamesOf(ctx context.Context, gear *gearuc.Service, activities ...domain.Activity) map[domain.GearID]string {
	if gear == nil {
		return nil
	}
	var ids []domain.GearID
	for _, a := range activities {
		if a.GearID != nil {
			ids = append(ids, *a.GearID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	names, err := gear.Names(ctx, ids)
	if err != nil {
		return nil
	}
	return names
}
