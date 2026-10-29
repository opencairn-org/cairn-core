package view

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeSegments struct {
	port.SegmentRepo
	efforts []domain.SegmentEffort
	segs    map[domain.SegmentID]domain.Segment
	similar []domain.SimilarActivity
}

func (f *fakeSegments) ListEffortsForActivity(context.Context, domain.ActivityID) ([]domain.SegmentEffort, error) {
	return f.efforts, nil
}
func (f *fakeSegments) GetSegment(_ context.Context, id domain.SegmentID) (domain.Segment, error) {
	return f.segs[id], nil
}
func (f *fakeSegments) FindSimilarActivities(context.Context, domain.ActivityID, domain.UserID, float64, float64, float64, string) ([]domain.SimilarActivity, error) {
	return f.similar, nil
}

type fakeStreams struct {
	port.StreamRepo
	samples []domain.StreamSample
}

func (f *fakeStreams) QueryStream(_ context.Context, q domain.StreamQuery) (domain.Stream, error) {
	return domain.Stream{ActivitySourceID: q.ActivitySourceID, Samples: f.samples}, nil
}

type fakeEngagement struct {
	port.EngagementRepo
	kudosers []domain.UserID
	remote   []domain.RemoteKudoser
	comments []domain.Comment
	rcs      []domain.RemoteComment
}

func (f *fakeEngagement) CountKudos(context.Context, domain.ActivityID) (int, error) {
	return len(f.kudosers), nil
}
func (f *fakeEngagement) HasKudos(_ context.Context, _ domain.ActivityID, me domain.UserID) (bool, error) {
	for _, k := range f.kudosers {
		if k == me {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeEngagement) ListKudosers(context.Context, domain.ActivityID, int) ([]domain.UserID, error) {
	return f.kudosers, nil
}
func (f *fakeEngagement) CountRemoteKudos(context.Context, domain.ActivityID) (int, error) {
	return len(f.remote), nil
}
func (f *fakeEngagement) ListRemoteKudosers(context.Context, domain.ActivityID, int) ([]domain.RemoteKudoser, error) {
	return f.remote, nil
}
func (f *fakeEngagement) ListComments(context.Context, domain.ActivityID, int, int) ([]domain.Comment, error) {
	return f.comments, nil
}
func (f *fakeEngagement) ListRemoteComments(context.Context, domain.ActivityID, int) ([]domain.RemoteComment, error) {
	return f.rcs, nil
}

type fakePresence struct{ workers []domain.LiveWorker }

func (f fakePresence) LiveWorkers(context.Context) []domain.LiveWorker { return f.workers }

func f64p(v float64) *float64 { return &v }

func TestDetailsLapsAndEfforts(t *testing.T) {
	repo := inmem.NewActivityRepo()
	user := domain.UserID(uuid.New())
	aid := domain.ActivityID(uuid.New())
	s1, s2 := domain.SourceID(uuid.New()), domain.SourceID(uuid.New())
	repo.Activities[aid] = domain.Activity{ID: aid, UserID: user, PrimaryStreamSourceID: &s1}
	repo.Sources[s1] = domain.ActivitySource{ID: s1, ActivityID: aid, UserID: user, Status: domain.SourceStatusActive, Parsed: domain.ActivitySourcePayload{Laps: []domain.ActivityLap{{}, {}}}}
	repo.Sources[s2] = domain.ActivitySource{ID: s2, ActivityID: aid, UserID: user, Status: domain.SourceStatusActive, Parsed: domain.ActivitySourcePayload{Laps: []domain.ActivityLap{{}}}}

	start := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	samples := make([]domain.StreamSample, 100)
	for i := range samples {
		samples[i] = domain.StreamSample{Timestamp: start.Add(time.Duration(i) * time.Second), DistanceM: f64p(float64(i * 10))}
	}
	segID := domain.SegmentID(uuid.New())
	segs := &fakeSegments{
		efforts: []domain.SegmentEffort{{SegmentID: segID, ActivityID: aid, ActivitySourceID: s1, StartOffset: 10, EndOffset: 50, ElapsedS: 40, PersonalRank: 1}},
		segs:    map[domain.SegmentID]domain.Segment{segID: {ID: segID, Name: "Hill", DistanceM: 400, ClimbCategory: domain.ClimbCategoryNone}},
	}
	d := NewDetails(repo, &fakeStreams{samples: samples}, segs, nil, nil, nil, nil, nil, nil)
	act := repo.Activities[aid]

	laps, sid, err := d.Laps(context.Background(), act)
	if err != nil || len(laps) != 2 || sid == nil || *sid != s1 {
		t.Fatalf("laps %d sid %v err %v", len(laps), sid, err)
	}
	rows, err := d.SegmentEfforts(context.Background(), act)
	if err != nil || len(rows) != 1 {
		t.Fatalf("efforts %v %v", rows, err)
	}
	r := rows[0]
	if r.Segment.Name != "Hill" || r.StartOffsetS == nil || *r.StartOffsetS != 10 || *r.EndOffsetS != 50 || *r.StartDistanceM != 100 || *r.EndDistanceM != 500 {
		t.Fatalf("row %+v", r)
	}
	if _, err := NewDetails(repo, nil, nil, nil, nil, nil, nil, nil, nil).SegmentEfforts(context.Background(), act); err == nil {
		t.Fatal("no segments repo must be unavailable")
	}
}

func TestDetailsZonesSimilarMergedManage(t *testing.T) {
	repo := inmem.NewActivityRepo()
	user := domain.UserID(uuid.New())
	aid := domain.ActivityID(uuid.New())
	s1 := domain.SourceID(uuid.New())
	d10 := 10000.0
	repo.Activities[aid] = domain.Activity{ID: aid, UserID: user, PrimaryStreamSourceID: &s1, StartLat: f64p(49), StartLng: f64p(8),
		Summary: domain.ActivitySummary{DistanceM: &d10}, MovingDuration: 50 * time.Minute, MergeProvenance: domain.MergeProvenance{domain.FieldGroupTitle: s1}}
	repo.Sources[s1] = domain.ActivitySource{ID: s1, ActivityID: aid, UserID: user, Status: domain.SourceStatusActive, Provider: "garmin",
		SourceWorkerPackage: "garmin", SourceWorkerVersion: "1.1", RawBlobID: "b", Parsed: domain.ActivitySourcePayload{HasStream: true}}
	start := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	hr := int16(150)
	samples := make([]domain.StreamSample, 700)
	for i := range samples {
		samples[i] = domain.StreamSample{Timestamp: start.Add(time.Duration(i) * time.Second), HeartRateBpm: &hr, SpeedMps: f64p(3)}
	}
	segs := &fakeSegments{similar: []domain.SimilarActivity{{ActivityID: domain.ActivityID(uuid.New()), StartTime: start.AddDate(0, 0, -1), DistanceM: &d10, MovingS: 2700, SharedSegments: 1, TargetSegments: 1}}}
	presence := fakePresence{workers: []domain.LiveWorker{{Provider: "garmin", Package: "garmin", Version: "1.2", Compatible: true}}}
	d := NewDetails(repo, &fakeStreams{samples: samples}, segs, nil, nil, nil, nil, presence, nil)
	act := repo.Activities[aid]

	// No athlete profile → HR has no reference → no zones, but no error.
	z, err := d.Zones(context.Background(), act)
	if err != nil || z.HR != nil || z.Power != nil {
		t.Fatalf("zones %+v %v", z, err)
	}
	g, err := d.Similar(context.Background(), act)
	if err != nil || len(g.Rows) != 2 || g.CurrentRank != 2 || g.TargetSegments != 1 {
		t.Fatalf("similar %+v %v", g, err)
	}
	merged, has, err := d.MergedStream(context.Background(), act)
	if err != nil || !has || len(merged.Grid) == 0 {
		t.Fatalf("merged has=%v grid=%d err=%v", has, len(merged.Grid), err)
	}
	info, err := d.Manage(context.Background(), act)
	if err != nil || len(info.Sources) != 1 || !info.Sources[0].ReparseEligible || info.Sources[0].Upgrade != "new_parser" || len(info.Provenance) != 1 {
		t.Fatalf("manage %+v %v", info, err)
	}
	src, err := d.ParsedSource(context.Background(), act, s1)
	if err != nil || src.ID != s1 {
		t.Fatalf("parsed %v %v", src, err)
	}
	if _, err := d.ParsedSource(context.Background(), act, domain.SourceID(uuid.New())); err == nil {
		t.Fatal("unknown source must fail")
	}
}

func TestDetailsKudosAndComments(t *testing.T) {
	repo := inmem.NewActivityRepo()
	me, friend := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	users := &fakeUsers{users: map[domain.UserID]domain.User{me: {ID: me, Username: "me"}, friend: {ID: friend, Username: "friend"}}}
	aid := domain.ActivityID(uuid.New())
	act := domain.Activity{ID: aid, UserID: me}
	eng := &fakeEngagement{
		kudosers: []domain.UserID{friend, me},
		remote:   []domain.RemoteKudoser{{ActorID: "https://o/u/x", Handle: "@x@o"}},
		comments: []domain.Comment{{ID: domain.CommentID(uuid.New()), UserID: friend, Body: "nice", CreatedAt: vNow.Add(time.Minute)}},
		rcs:      []domain.RemoteComment{{ID: "https://o/n/1", Handle: "@x@o", ActorID: "https://o/u/x", Body: "wow", CreatedAt: vNow}},
	}
	d := NewDetails(repo, nil, nil, nil, eng, users, nil, nil, NewVisibility(nil, nil, nil))
	if !d.SocialAllowed(context.Background(), act, me) {
		t.Fatal("owner is always allowed")
	}
	v, err := d.Kudos(context.Background(), act, me)
	if err != nil || v.Count != 3 || !v.HasKudos || len(v.Kudosers) != 3 || !v.Kudosers[2].Remote || v.Kudosers[0].User.Username != "friend" {
		t.Fatalf("kudos %+v %v", v, err)
	}
	cs, err := d.Comments(context.Background(), act)
	if err != nil || len(cs) != 2 || cs[0].Body != "wow" || !cs[0].Author.Remote || cs[1].Author.User.Username != "friend" {
		t.Fatalf("comments %+v %v", cs, err)
	}
}
