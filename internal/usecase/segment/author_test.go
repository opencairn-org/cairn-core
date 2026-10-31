package segment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeSegRepo struct {
	port.SegmentReader
	port.SegmentWriter
	segs    map[domain.SegmentID]domain.Segment
	efforts map[domain.SegmentID]int
	owns    bool
	deleted []domain.SegmentID
}

func (f *fakeSegRepo) SaveSegment(_ context.Context, s domain.Segment) error {
	f.segs[s.ID] = s
	return nil
}
func (f *fakeSegRepo) GetSegment(_ context.Context, id domain.SegmentID) (domain.Segment, error) {
	s, ok := f.segs[id]
	if !ok {
		return domain.Segment{}, domain.ErrNotFound
	}
	return s, nil
}
func (f *fakeSegRepo) DeleteSegment(_ context.Context, id domain.SegmentID) error {
	delete(f.segs, id)
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeSegRepo) CountEffortsForSegment(_ context.Context, id domain.SegmentID) (int, error) {
	return f.efforts[id], nil
}
func (f *fakeSegRepo) ViewerOwnsAccount(context.Context, domain.UserID, domain.ExternalAccountID) (bool, error) {
	return f.owns, nil
}

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

// line is a ~1.1 km northbound polyline (precision 5).
var line = encodePolyline5([]domain.GeoPoint{{Lat: 48.0, Lon: 11.0}, {Lat: 48.01, Lon: 11.0}})

func encodePolyline5(points []domain.GeoPoint) string {
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

func TestAuthoringLifecycle(t *testing.T) {
	repo := &fakeSegRepo{segs: map[domain.SegmentID]domain.Segment{}, efforts: map[domain.SegmentID]int{}}
	a := &Authoring{Segments: repo, Now: func() time.Time { return time.Unix(0, 0).UTC() }}
	owner, stranger := domain.UserID(uuid.New()), domain.UserID(uuid.New())

	if _, err := a.Create(t.Context(), owner, domain.NativeSegmentInput{ActivityType: domain.ActivityTypeRide, Polyline: line}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("no name: %v", err)
	}
	seg, err := a.Create(t.Context(), owner, domain.NativeSegmentInput{Name: "Hill", ActivityType: domain.ActivityTypeRide, Polyline: line})
	if err != nil || seg.Source != domain.SegmentSourceNative || *seg.OwnerUserID != owner {
		t.Fatalf("create: %v %+v", err, seg)
	}
	name := "Renamed"
	if _, err := a.Update(t.Context(), stranger, seg.ID, domain.SegmentPatch{Name: &name}); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("stranger edits private: %v", err)
	}
	inst := domain.SegmentScopeInstance
	if _, err := a.Update(t.Context(), owner, seg.ID, domain.SegmentPatch{Scope: &inst}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(t.Context(), stranger, seg.ID, domain.SegmentPatch{Name: &name}); kindOf(t, err) != port.ActionErrorForbidden {
		t.Fatalf("stranger edits instance: %v", err)
	}
	repo.efforts[seg.ID] = 3
	if _, err := a.Update(t.Context(), owner, seg.ID, domain.SegmentPatch{Polyline: &[]string{line}[0]}); kindOf(t, err) != port.ActionErrorPrecondition {
		t.Fatalf("locked polyline: %v", err)
	}
	got, err := a.Update(t.Context(), owner, seg.ID, domain.SegmentPatch{Name: &name})
	if err != nil || got.Name != "Renamed" || repo.segs[seg.ID].Name != "Renamed" {
		t.Fatalf("rename: %v %+v", err, got)
	}
	if err := a.Delete(t.Context(), owner, seg.ID, true); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("keep efforts: %v", err)
	}
	if err := a.Delete(t.Context(), stranger, seg.ID, false); kindOf(t, err) != port.ActionErrorForbidden {
		t.Fatalf("stranger delete: %v", err)
	}
	if err := a.Delete(t.Context(), owner, seg.ID, false); err != nil || len(repo.deleted) != 1 {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := a.RematchActivity(t.Context(), owner, domain.ActivityID(uuid.New())); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("no matcher: %v", err)
	}
}
