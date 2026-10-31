package connect

import (
	"context"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	segmentuc "github.com/opencairn-org/cairn-core/internal/usecase/segment"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

type fakeSegAuthorRepo struct {
	port.SegmentRepo
	segs   map[domain.SegmentID]domain.Segment
	name   string
	listed domain.SegmentListFilter
}

func (f *fakeSegAuthorRepo) SaveSegment(_ context.Context, s domain.Segment) error {
	f.segs[s.ID] = s
	return nil
}
func (f *fakeSegAuthorRepo) GetSegment(_ context.Context, id domain.SegmentID) (domain.Segment, error) {
	s, ok := f.segs[id]
	if !ok {
		return domain.Segment{}, domain.ErrNotFound
	}
	return s, nil
}
func (f *fakeSegAuthorRepo) DeleteSegment(_ context.Context, id domain.SegmentID) error {
	delete(f.segs, id)
	return nil
}
func (f *fakeSegAuthorRepo) CountEffortsForSegment(context.Context, domain.SegmentID) (int, error) {
	return 0, nil
}
func (f *fakeSegAuthorRepo) ViewerOwnsAccount(context.Context, domain.UserID, domain.ExternalAccountID) (bool, error) {
	return false, nil
}
func (f *fakeSegAuthorRepo) ListSegments(_ context.Context, _ domain.UserID, fl domain.SegmentListFilter) ([]domain.Segment, int, error) {
	f.listed = fl
	var out []domain.Segment
	for _, s := range f.segs {
		out = append(out, s)
	}
	return out, len(out), nil
}
func (f *fakeSegAuthorRepo) SearchSegmentsInBBox(context.Context, domain.UserID, port.BoundingBox, []domain.ActivityType, bool, int) ([]domain.Segment, error) {
	var out []domain.Segment
	for _, s := range f.segs {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeSegAuthorRepo) ListSegmentEffortsFiltered(_ context.Context, _ domain.SegmentID, viewer domain.UserID, q domain.SegmentEffortQuery) ([]domain.SegmentEffortCandidate, error) {
	if q.Offset > 0 {
		return nil, nil
	}
	return []domain.SegmentEffortCandidate{{
		Effort:      domain.SegmentEffort{ID: domain.SegmentEffortID(uuid.New()), UserID: viewer, ElapsedS: 10},
		Activity:    domain.Activity{UserID: viewer},
		DisplayName: f.name,
	}}, nil
}

func TestSegmentAuthoringRPCs(t *testing.T) {
	repo := &fakeSegAuthorRepo{segs: map[domain.SegmentID]domain.Segment{}}
	var backfills []domain.SegmentID
	s := NewSegmentServer(view.NewSegments(repo, nil))
	s.Authoring = &segmentuc.Authoring{Segments: repo, Now: func() time.Time { return actNow }}
	s.Backfill = func(_ domain.UserID, id domain.SegmentID, _, _ time.Time) string {
		backfills = append(backfills, id)
		return "rematch-segment:" + id.String()
	}
	me := domain.UserID(uuid.New())
	ctx := context.WithValue(t.Context(), ctxKeyUserID, me)
	line := encodePolyline5ForTest([]domain.GeoPoint{{Lat: 48.0, Lon: 11.0}, {Lat: 48.01, Lon: 11.0}})

	if _, err := (&SegmentServer{Segments: s.Segments}).CreateSegment(ctx, &v1.CreateSegmentRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("unwired: %v", err)
	}
	if _, err := s.CreateSegment(ctx, &v1.CreateSegmentRequest{Name: "x"}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("no type: %v", err)
	}
	created, err := s.CreateSegment(ctx, &v1.CreateSegmentRequest{
		Name: "Hill", ActivityType: v1.ActivityType_ACTIVITY_TYPE_RIDE, Polyline: &v1.EncodedPolyline{Encoded: line, Precision: 5},
		Scope: v1.SegmentScope_SEGMENT_SCOPE_INSTANCE, BackfillEfforts: true,
	})
	if err != nil || created.GetSegment().GetScope() != v1.SegmentScope_SEGMENT_SCOPE_INSTANCE || created.GetBackfillJobId() == "" || len(backfills) != 1 {
		t.Fatalf("create: %v %v", created, err)
	}
	id := created.GetSegment().GetId()

	list, err := s.ListSegments(ctx, &v1.ListSegmentsRequest{Sources: []string{"cairn"}, Sort: v1.SegmentSort_SEGMENT_SORT_DISTANCE_DESC})
	if err != nil || len(list.GetSegments()) != 1 || list.GetPage().GetTotal() != 1 || repo.listed.Sources[0] != domain.SegmentSourceNative || repo.listed.Sort != domain.SegmentSortDistanceDesc {
		t.Fatalf("list: %v %v %+v", list, err, repo.listed)
	}
	box, err := s.SearchSegmentsInBoundingBox(ctx, &v1.SearchSegmentsInBoundingBoxRequest{Bbox: &v1.BoundingBox{MinLatitude: 47, MaxLatitude: 49, MinLongitude: 10, MaxLongitude: 12}})
	if err != nil || len(box.GetSegments()) != 1 {
		t.Fatalf("bbox: %v %v", box, err)
	}
	if _, err := s.SearchSegmentsInBoundingBox(ctx, &v1.SearchSegmentsInBoundingBoxRequest{}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("no bbox: %v", err)
	}
	name := "Renamed"
	upd, err := s.UpdateSegment(ctx, &v1.UpdateSegmentRequest{Id: id, FieldMask: []string{"name"}, Name: &name})
	if err != nil || upd.GetSegment().GetName() != "Renamed" {
		t.Fatalf("update: %v %v", upd, err)
	}
	if _, err := s.UpdateSegment(ctx, &v1.UpdateSegmentRequest{Id: id, FieldMask: []string{"owner"}}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("bad mask: %v", err)
	}
	efforts, err := s.ListSegmentEfforts(ctx, &v1.ListSegmentEffortsRequest{SegmentId: id})
	if err != nil || len(efforts.GetEfforts()) != 1 {
		t.Fatalf("efforts: %v %v", efforts, err)
	}
	repo.name = "Me"
	board, err := s.GetSegmentLeaderboard(ctx, &v1.GetSegmentLeaderboardRequest{SegmentId: id})
	if err != nil || len(board.GetEntries()) != 1 || board.GetEntries()[0].GetUserDisplayName() != "Me" {
		t.Fatalf("board: %v %v", board, err)
	}
	sex := v1.BiologicalSex_BIOLOGICAL_SEX_FEMALE
	if _, err := s.GetSegmentLeaderboard(ctx, &v1.GetSegmentLeaderboardRequest{SegmentId: id, BiologicalSex: &sex}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("sex filter: %v", err)
	}
	rem, err := s.RematchSegment(ctx, &v1.RematchSegmentRequest{SegmentId: id})
	if err != nil || rem.GetJobId() == "" || len(backfills) != 2 {
		t.Fatalf("rematch: %v %v", rem, err)
	}
	if _, err := s.DeleteSegment(ctx, &v1.DeleteSegmentRequest{Id: id, KeepEfforts: true}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("keep efforts: %v", err)
	}
	if _, err := s.DeleteSegment(ctx, &v1.DeleteSegmentRequest{Id: id}); err != nil || len(repo.segs) != 0 {
		t.Fatalf("delete: %v", err)
	}
}

func encodePolyline5ForTest(points []domain.GeoPoint) string {
	var out []byte
	var prevLat, prevLon int64
	enc := func(v int64) {
		v <<= 1
		if v < 0 {
			v = ^v
		}
		for v >= 0x20 {
			out = append(out, byte((0x20|(v&0x1f))+63))
			v >>= 5
		}
		out = append(out, byte(v+63))
	}
	for _, p := range points {
		lat, lon := int64(p.Lat*1e5+0.5), int64(p.Lon*1e5+0.5)
		enc(lat - prevLat)
		enc(lon - prevLon)
		prevLat, prevLon = lat, lon
	}
	return string(out)
}
