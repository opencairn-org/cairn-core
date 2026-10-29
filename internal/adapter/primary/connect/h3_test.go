package connect

import (
	"context"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

type fakeSegmentsH3 struct {
	port.SegmentRepo
	stats   domain.UserSegmentStats
	items   []domain.UserSegmentListItem
	seg     domain.Segment
	efforts []domain.SegmentEffort
}

func (f *fakeSegmentsH3) UserSegmentStats(context.Context, domain.UserID) (domain.UserSegmentStats, error) {
	return f.stats, nil
}
func (f *fakeSegmentsH3) ListUserSegmentsByEffortCount(_ context.Context, _ domain.UserID, _ string, limit, offset int) ([]domain.UserSegmentListItem, error) {
	if offset >= len(f.items) {
		return nil, nil
	}
	return f.items[offset:min(offset+limit, len(f.items))], nil
}
func (f *fakeSegmentsH3) GetSegment(_ context.Context, id domain.SegmentID) (domain.Segment, error) {
	if id != f.seg.ID {
		return domain.Segment{}, domain.ErrNotFound
	}
	return f.seg, nil
}
func (f *fakeSegmentsH3) ListEffortsForSegment(_ context.Context, _ domain.SegmentID, _ domain.UserID, _ port.LeaderboardScope, limit, offset int) ([]domain.SegmentEffort, error) {
	if offset >= len(f.efforts) {
		return nil, nil
	}
	return f.efforts[offset:min(offset+limit, len(f.efforts))], nil
}

type fakeGeoH3 struct {
	port.StreamRepo
	tracks map[domain.SourceID][]domain.GeoPoint
}

func (f *fakeGeoH3) QueryGeoTracks(_ context.Context, ids []domain.SourceID, _ domain.StreamResolution) (map[domain.SourceID][]domain.GeoPoint, error) {
	out := map[domain.SourceID][]domain.GeoPoint{}
	for _, id := range ids {
		out[id] = f.tracks[id]
	}
	return out, nil
}

type fakeAthlete struct {
	port.AthleteProfileRepo
	entries []domain.AthleteMetricEntry
	deleted []domain.AthleteMetricID
}

func (f *fakeAthlete) ListEntries(context.Context, domain.UserID) ([]domain.AthleteMetricEntry, error) {
	return f.entries, nil
}
func (f *fakeAthlete) UpsertEntry(_ context.Context, e domain.AthleteMetricEntry) (domain.AthleteMetricID, error) {
	e.ID = domain.AthleteMetricID(uuid.New())
	f.entries = append(f.entries, e)
	return e.ID, nil
}
func (f *fakeAthlete) DeleteEntry(_ context.Context, _ domain.UserID, id domain.AthleteMetricID) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeSettings struct {
	port.UserSettingsRepo
	policies map[domain.ActivityType]domain.MergePolicy
}

func (f *fakeSettings) GetAllMergePolicies(context.Context, domain.UserID) (map[domain.ActivityType]domain.MergePolicy, error) {
	return f.policies, nil
}
func (f *fakeSettings) SetMergePolicies(_ context.Context, _ domain.UserID, p map[domain.ActivityType]domain.MergePolicy) error {
	f.policies = p
	return nil
}

func TestStatsServerStatsAndHeatmap(t *testing.T) {
	repo := inmem.NewActivityRepo()
	user := domain.UserID(uuid.New())
	sid := domain.SourceID(uuid.New())
	id := seedPlain(repo, user, domain.ActivityTypeRide, actNow.Add(-time.Hour), domain.PrivacyPrivate)
	a := repo.Activities[id]
	a.PrimaryStreamSourceID = &sid
	repo.Activities[id] = a
	geo := &fakeGeoH3{tracks: map[domain.SourceID][]domain.GeoPoint{sid: {{Lat: 49.0, Lon: 8.0}, {Lat: 49.001, Lon: 8.001}}}}
	s := NewStatsServer(nil, &fakeContent{}, nil, func(domain.Activity) string { return "e" })
	s.Stats = view.NewStats(repo, nil, nil, geo, func() time.Time { return actNow })
	ctx := context.WithValue(t.Context(), ctxKeyUserID, user)

	st, err := s.GetStats(ctx, &v1.GetStatsRequest{})
	if err != nil || st.GetTotals().GetCount() != 1 || st.GetLongestDistance().GetId() != id.String() || len(st.GetByYear()) != 1 {
		t.Fatalf("stats %v %v", st, err)
	}
	hm, err := s.GetHeatmap(ctx, &v1.GetHeatmapRequest{Resolution: v1.GetHeatmapRequest_RESOLUTION_5S})
	if err != nil || hm.GetMatched() != 1 || len(hm.GetTracks()) != 1 || hm.GetTracks()[0].GetPolyline().GetEncoded() == "" || hm.GetTracks()[0].GetPolyline().GetPrecision() != 5 {
		t.Fatalf("heatmap %v %v", hm, err)
	}
	if _, err := s.GetTrainingLoad(ctx, &v1.GetTrainingLoadRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("no metrics repo: %v", err)
	}
	if _, err := s.GetBestEffortHistory(ctx, &v1.GetBestEffortHistoryRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("no best efforts repo: %v", err)
	}
}

func TestSegmentServer(t *testing.T) {
	user := domain.UserID(uuid.New())
	segID := domain.SegmentID(uuid.New())
	f := &fakeSegmentsH3{
		stats: domain.UserSegmentStats{Segments: 2, Efforts: 5, PRs: 1},
		items: []domain.UserSegmentListItem{{ID: segID, Name: "Hill", ActivityType: "ride", Source: domain.SegmentSourceExternal, EffortCount: 5, HasPR: true}, {ID: domain.SegmentID(uuid.New()), Name: "Flat"}},
		seg:   domain.Segment{ID: segID, Name: "Hill", Source: domain.SegmentSourceExternal, Scope: domain.SegmentScopeInstance, ActivityType: domain.ActivityTypeRide, Polyline: "abc", PolylinePrecision: 5, DistanceM: 1200, ClimbCategory: domain.ClimbCategory3},
		efforts: []domain.SegmentEffort{
			{ID: domain.SegmentEffortID(uuid.New()), SegmentID: segID, UserID: user, ElapsedS: 300, PersonalRank: 1, IsPersonalRecord: true, StartTime: actNow.Add(-24 * time.Hour)},
			{ID: domain.SegmentEffortID(uuid.New()), SegmentID: segID, UserID: user, ElapsedS: 320, PersonalRank: 2, StartTime: actNow},
		},
	}
	s := NewSegmentServer(view.NewSegments(f))
	ctx := context.WithValue(t.Context(), ctxKeyUserID, user)

	mine, err := s.ListMySegments(ctx, &v1.ListMySegmentsRequest{Page: &v1.PageRequest{Limit: 1}})
	if err != nil || mine.GetStats().GetSegments() != 2 || len(mine.GetSegments()) != 1 || mine.GetPage().GetNextCursor() != "1" || !mine.GetSegments()[0].GetHasPr() {
		t.Fatalf("mine %v %v", mine, err)
	}
	second, _ := s.ListMySegments(ctx, &v1.ListMySegmentsRequest{Page: &v1.PageRequest{Limit: 1, Cursor: "1"}})
	if second.Stats != nil || len(second.GetSegments()) != 1 || second.GetPage().GetNextCursor() != "" {
		t.Fatalf("second page %v", second)
	}
	got, err := s.GetSegment(ctx, &v1.GetSegmentRequest{Id: segID.String(), IncludeTopEfforts: true})
	if err != nil || got.GetSegment().GetName() != "Hill" || got.GetSegment().GetClimbCategory() != v1.ClimbCategory_CLIMB_CATEGORY_THREE || len(got.GetTopEfforts()) != 2 {
		t.Fatalf("segment %v %v", got, err)
	}
	ef, err := s.ListMySegmentEfforts(ctx, &v1.ListMySegmentEffortsRequest{SegmentId: segID.String()})
	if err != nil || len(ef.GetEfforts()) != 2 || ef.GetPersonalRecord() == nil || ef.GetPersonalRecord().GetElapsed().GetSeconds() != 300 {
		t.Fatalf("efforts %v %v", ef, err)
	}
	// A stranger with no efforts cannot see the segment.
	f.efforts = nil
	_, err = s.GetSegment(ctx, &v1.GetSegmentRequest{Id: segID.String()})
	wantCode(t, err, connectrpc.CodeNotFound)
	_, err = s.GetSegment(ctx, &v1.GetSegmentRequest{Id: "nope"})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
}

func TestMetricsServerAthleteAndHealth(t *testing.T) {
	user := domain.UserID(uuid.New())
	athlete := &fakeAthlete{}
	s := NewMetricsServer(nil, nil, athlete, nil)
	ctx := context.WithValue(t.Context(), ctxKeyUserID, user)

	up, err := s.UpsertAthleteMetric(ctx, &v1.UpsertAthleteMetricRequest{Key: "ftp_watts", EffectiveDate: "2026-10-01", Value: 250})
	if err != nil || up.GetId() == "" {
		t.Fatalf("upsert %v %v", up, err)
	}
	_, err = s.UpsertAthleteMetric(ctx, &v1.UpsertAthleteMetricRequest{Key: "ftp_watts", EffectiveDate: "yesterday", Value: 250})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	_, err = s.UpsertAthleteMetric(ctx, &v1.UpsertAthleteMetricRequest{Key: "ftp_watts", EffectiveDate: "2026-10-01", Value: 9999})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	list, err := s.ListAthleteMetrics(ctx, &v1.ListAthleteMetricsRequest{})
	if err != nil || len(list.GetEntries()) != 1 || list.GetEntries()[0].GetEffectiveDate() != "2026-10-01" || len(list.GetSpecs()) != len(domain.AthleteMetricSpecs) {
		t.Fatalf("list %v %v", list, err)
	}
	if _, err := s.DeleteAthleteMetric(ctx, &v1.DeleteAthleteMetricRequest{Id: up.GetId()}); err != nil || len(athlete.deleted) != 1 {
		t.Fatalf("delete %v %v", err, athlete.deleted)
	}
	_, err = s.ListHealthSeries(ctx, &v1.ListHealthSeriesRequest{Type: "HRV"})
	wantCode(t, err, connectrpc.CodeUnavailable)
	_, err = s.ListMetrics(ctx, &v1.ListMetricsRequest{})
	wantCode(t, err, connectrpc.CodeUnavailable)
}

func TestSettingsServerMergePolicies(t *testing.T) {
	user := domain.UserID(uuid.New())
	store := &fakeSettings{policies: map[domain.ActivityType]domain.MergePolicy{}}
	s := NewSettingsServer(store)
	ctx := context.WithValue(t.Context(), ctxKeyUserID, user)

	got, err := s.GetSettings(ctx, &v1.GetSettingsRequest{})
	if err != nil || got.GetSettings().GetUserId() != user.String() || len(got.GetSettings().GetMergePolicyByActivityType()) != 0 {
		t.Fatalf("empty %v %v", got, err)
	}
	upd, err := s.UpdateSettings(ctx, &v1.UpdateSettingsRequest{FieldMask: []string{"merge_policy_by_activity_type"}, Settings: &v1.UserSettings{
		MergePolicyByActivityType: map[string]*v1.ActivityTypeMergePolicy{"ride": {DefaultProviders: []string{"garmin", "strava"},
			ByFieldGroup: map[string]*v1.ProviderPriorityList{"gps_track": {Providers: []string{"strava"}}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := upd.GetSettings().GetMergePolicyByActivityType()["ride"]
	if p == nil || len(p.GetDefaultProviders()) != 2 || p.GetByFieldGroup()["gps_track"].GetProviders()[0] != "strava" {
		t.Fatalf("updated %v", upd)
	}
	if store.policies[domain.ActivityTypeRide].Overrides[domain.FieldGroupGPSTrack][0] != "strava" {
		t.Fatalf("stored %+v", store.policies)
	}
	_, err = s.UpdateSettings(ctx, &v1.UpdateSettingsRequest{FieldMask: []string{"height_cm"}})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	_, err = s.UpdateSettings(ctx, &v1.UpdateSettingsRequest{FieldMask: []string{"merge_policy_by_activity_type"}, Settings: &v1.UserSettings{
		MergePolicyByActivityType: map[string]*v1.ActivityTypeMergePolicy{"ride": {ByFieldGroup: map[string]*v1.ProviderPriorityList{"nope": {}}}}}})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	rs, err := s.ResetSettingsSection(ctx, &v1.ResetSettingsSectionRequest{Section: v1.SettingsSection_SETTINGS_SECTION_MERGE_POLICIES})
	if err != nil || len(rs.GetSettings().GetMergePolicyByActivityType()) != 0 || len(store.policies) != 0 {
		t.Fatalf("reset %v %v", rs, err)
	}
	_, err = s.ResetSettingsSection(ctx, &v1.ResetSettingsSectionRequest{Section: v1.SettingsSection_SETTINGS_SECTION_PROFILE})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
}
