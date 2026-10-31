package connect

import (
	"context"
	"math"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

type fakeSegmentsD struct {
	port.SegmentReader
	efforts []domain.SegmentEffort
	segs    map[domain.SegmentID]domain.Segment
}

func (f *fakeSegmentsD) ListEffortsForActivity(context.Context, domain.ActivityID) ([]domain.SegmentEffort, error) {
	return f.efforts, nil
}
func (f *fakeSegmentsD) GetSegment(_ context.Context, id domain.SegmentID) (domain.Segment, error) {
	return f.segs[id], nil
}
func (f *fakeSegmentsD) FindSimilarActivities(context.Context, domain.SimilarRouteQuery) ([]domain.SimilarActivity, error) {
	return nil, nil
}

type fakeEngagementD struct {
	port.EngagementRepo
	kudosers []domain.UserID
}

func (f *fakeEngagementD) CountKudos(context.Context, domain.ActivityID) (int, error) {
	return len(f.kudosers), nil
}
func (f *fakeEngagementD) HasKudos(context.Context, domain.ActivityID, domain.UserID) (bool, error) {
	return false, nil
}
func (f *fakeEngagementD) ListKudosers(context.Context, domain.ActivityID, int) ([]domain.UserID, error) {
	return f.kudosers, nil
}
func (f *fakeEngagementD) CountRemoteKudos(context.Context, domain.ActivityID) (int, error) {
	return 0, nil
}
func (f *fakeEngagementD) ListRemoteKudosers(context.Context, domain.ActivityID, int) ([]domain.RemoteKudoser, error) {
	return nil, nil
}
func (f *fakeEngagementD) ListComments(context.Context, domain.ActivityID, int, int) ([]domain.Comment, error) {
	return []domain.Comment{{ID: domain.CommentID(uuid.New()), Body: "hi", CreatedAt: actNow}}, nil
}
func (f *fakeEngagementD) ListRemoteComments(context.Context, domain.ActivityID, int) ([]domain.RemoteComment, error) {
	return nil, nil
}

func detailFixture(t *testing.T) (*actFixture, domain.ActivityID, domain.SourceID) {
	t.Helper()
	f := newActFixture(t)
	sid, aid := seedActivity(f.repo, f.user, "garmin", actNow, 10000)
	act := f.repo.Activities[aid]
	act.PrimaryStreamSourceID = &sid
	act.MergeProvenance = domain.MergeProvenance{domain.FieldGroupTitle: sid}
	f.repo.Activities[aid] = act
	src := f.repo.Sources[sid]
	src.Parsed.HasStream = true
	src.Parsed.Laps = []domain.ActivityLap{{Label: "L1", ElapsedDuration: time.Minute, StartOffset: 0}, {Label: "L2", ElapsedDuration: time.Minute, StartOffset: time.Minute}}
	src.SourceWorkerPackage, src.SourceWorkerVersion = "garmin", "1.1"
	f.repo.Sources[sid] = src
	samples := make([]domain.StreamSample, 120)
	hr := int16(140)
	for i := range samples {
		samples[i] = domain.StreamSample{Timestamp: actNow.Add(time.Duration(i) * time.Second), HeartRateBpm: &hr, DistanceM: f64(float64(i))}
	}
	segID := domain.SegmentID(uuid.New())
	segs := &fakeSegmentsD{
		efforts: []domain.SegmentEffort{{SegmentID: segID, ActivitySourceID: sid, StartOffset: 5, EndOffset: 60, ElapsedS: 55}},
		segs:    map[domain.SegmentID]domain.Segment{segID: {ID: segID, Name: "Hill", ClimbCategory: domain.ClimbCategoryHorsCategorie}},
	}
	users := &fakeUsers{users: map[domain.UserID]domain.User{f.user: {ID: f.user, Username: "me"}}}
	f.srv.Details = view.NewDetails(f.repo, &fakeStreams{samples: samples}, segs, nil, &fakeEngagementD{kudosers: []domain.UserID{f.user}}, users, nil, nil, view.NewVisibility(nil, nil, nil))
	return f, aid, sid
}

func TestListLapsAndParsedSource(t *testing.T) {
	f, aid, sid := detailFixture(t)
	res, err := f.srv.ListLaps(f.ctx, &v1.ListLapsRequest{ActivityId: aid.String()})
	if err != nil || len(res.GetLaps()) != 2 || res.GetSourceId() != sid.String() {
		t.Fatalf("laps %v %v", res, err)
	}
	if res.GetLaps()[1].GetIndex() != 2 || res.GetLaps()[1].GetStartOffsetS() != 60 || res.GetLaps()[1].GetLabel() != "L2" {
		t.Fatalf("lap %v", res.GetLaps()[1])
	}
	ps, err := f.srv.GetParsedSource(f.ctx, &v1.GetParsedSourceRequest{ActivityId: aid.String(), SourceId: sid.String()})
	if err != nil || ps.GetPayload().GetType() != v1.ActivityType_ACTIVITY_TYPE_RIDE || len(ps.GetPayload().GetLaps()) != 2 || ps.GetPayload().GetStreamSummary() == nil {
		t.Fatalf("parsed %v %v", ps, err)
	}
	_, err = f.srv.GetParsedSource(f.ctx, &v1.GetParsedSourceRequest{ActivityId: aid.String(), SourceId: uuid.NewString()})
	wantCode(t, err, connectrpc.CodeNotFound)
	// Not the owner.
	other := context.WithValue(t.Context(), ctxKeyUserID, domain.UserID(uuid.New()))
	_, err = f.srv.ListLaps(other, &v1.ListLapsRequest{ActivityId: aid.String()})
	wantCode(t, err, connectrpc.CodeNotFound)
}

func TestZonesEffortsMergedManage(t *testing.T) {
	f, aid, sid := detailFixture(t)
	z, err := f.srv.GetActivityZones(f.ctx, &v1.GetActivityZonesRequest{ActivityId: aid.String()})
	if err != nil || z.Hr != nil || z.Power != nil || z.Decoupling != nil {
		t.Fatalf("zones without thresholds %v %v", z, err)
	}
	ef, err := f.srv.ListActivitySegmentEfforts(f.ctx, &v1.ListActivitySegmentEffortsRequest{ActivityId: aid.String()})
	if err != nil || len(ef.GetEfforts()) != 1 {
		t.Fatalf("efforts %v %v", ef, err)
	}
	e := ef.GetEfforts()[0]
	if e.GetSegmentName() != "Hill" || e.GetClimbCategory() != "hors_categorie" || e.GetStartOffsetS() != 5 || e.GetEndOffsetS() != 60 || e.GetEndDistanceM() != 60 {
		t.Fatalf("effort %v", e)
	}
	sim, err := f.srv.ListSimilarActivities(f.ctx, &v1.ListSimilarActivitiesRequest{ActivityId: aid.String()})
	if err != nil || len(sim.GetActivities()) != 1 || !sim.GetActivities()[0].GetIsCurrent() || sim.GetCurrentRank() != 1 {
		t.Fatalf("similar %v %v", sim, err)
	}
	ms, err := f.srv.GetMergedStream(f.ctx, &v1.GetMergedStreamRequest{ActivityId: aid.String()})
	if err != nil || len(ms.GetGridUnixS()) == 0 {
		t.Fatalf("merged %v %v", ms, err)
	}
	if ch, ok := ms.GetChannels()["heart_rate"]; !ok || ch.GetSourceId() != sid.String() || len(ch.GetValues()) != len(ms.GetGridUnixS()) {
		t.Fatalf("merged channel %v", ms.GetChannels())
	}
	mi, err := f.srv.GetActivityManageInfo(f.ctx, &v1.GetActivityManageInfoRequest{ActivityId: aid.String()})
	if err != nil || len(mi.GetSources()) != 1 || mi.GetPrimarySourceId() != sid.String() || len(mi.GetProvenance()) != 1 {
		t.Fatalf("manage %v %v", mi, err)
	}
	if s := mi.GetSources()[0]; !s.GetIsPrimary() || s.GetParserVersion() != "1.1" || s.GetDatasetVersion() != "1" || s.GetLapCount() != 2 || !s.GetHasStream() || s.GetReparseEligible() {
		t.Fatalf("insight %v", s)
	}
}

func TestKudosAndCommentsGate(t *testing.T) {
	f, aid, _ := detailFixture(t)
	k, err := f.srv.ListKudos(f.ctx, &v1.ListKudosRequest{ActivityId: aid.String()})
	if err != nil || k.GetCount() != 1 || len(k.GetKudosers()) != 1 || k.GetKudosers()[0].GetUsername() != "me" {
		t.Fatalf("kudos %v %v", k, err)
	}
	c, err := f.srv.ListComments(f.ctx, &v1.ListCommentsRequest{ActivityId: aid.String()})
	if err != nil || len(c.GetComments()) != 1 || c.GetComments()[0].GetBody() != "hi" {
		t.Fatalf("comments %v %v", c, err)
	}
	// A stranger on a private activity: the social category is closed.
	other := context.WithValue(t.Context(), ctxKeyUserID, domain.UserID(uuid.New()))
	_, err = f.srv.ListKudos(other, &v1.ListKudosRequest{ActivityId: aid.String()})
	wantCode(t, err, connectrpc.CodePermissionDenied)
	_, err = f.srv.ListComments(t.Context(), &v1.ListCommentsRequest{ActivityId: aid.String()})
	wantCode(t, err, connectrpc.CodeUnauthenticated)
	f.srv.Details = nil
	_, err = f.srv.ListKudos(f.ctx, &v1.ListKudosRequest{ActivityId: aid.String()})
	wantCode(t, err, connectrpc.CodeUnavailable)
}

func TestMergedStreamNaN(t *testing.T) {
	sid := domain.SourceID(uuid.New())
	v := 1.5
	out := mergedStreamToProto(streamalignFixture(sid, []*float64{&v, nil}))
	if len(out.GetGridUnixS()) != 2 || out.GetChannels()["speed"].GetValues()[0] != 1.5 || !math.IsNaN(out.GetChannels()["speed"].GetValues()[1]) {
		t.Fatalf("merged %v", out)
	}
}
