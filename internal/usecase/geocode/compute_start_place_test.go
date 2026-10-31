package geocode

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// fakeActivityRepo embeds the port interface so unused methods are nil; only
// the two the use case calls are implemented. Captures the last SetStartLocation.
type fakeActivityRepo struct {
	port.ActivityGeoStore
	setCalled bool
	lat, lng  *float64
	place     string
}

func (f *fakeActivityRepo) SetStartLocation(_ context.Context, _ domain.ActivityID, lat, lng *float64, place string) error {
	f.setCalled = true
	f.lat, f.lng, f.place = lat, lng, place
	return nil
}

type fakeStreamRepo struct {
	port.StreamRepo
	lat, lon float64
	found    bool
	err      error
}

func (f *fakeStreamRepo) FirstGeoPoint(_ context.Context, _ domain.SourceID) (float64, float64, bool, error) {
	return f.lat, f.lon, f.found, f.err
}

type fakeGeocoder struct {
	place port.Place
	err   error
}

func (f *fakeGeocoder) ReverseGeocode(_ context.Context, _, _ float64) (port.Place, error) {
	return f.place, f.err
}

func cand() port.StartPlaceCandidate {
	return port.StartPlaceCandidate{
		ActivityID:            domain.ActivityID(uuid.New()),
		PrimaryStreamSourceID: domain.SourceID(uuid.New()),
	}
}

func TestExecuteForActivity_NoGPS_MarksAttempted(t *testing.T) {
	ar := &fakeActivityRepo{}
	sr := &fakeStreamRepo{found: false}
	uc := NewComputeStartPlace(ar, sr, &fakeGeocoder{}, nil)

	if err := uc.ExecuteForActivity(context.Background(), cand()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ar.setCalled {
		t.Fatal("expected SetStartLocation to be called")
	}
	if ar.lat != nil || ar.lng != nil || ar.place != "" {
		t.Fatalf("no-GPS should store (nil,nil,\"\"), got (%v,%v,%q)", ar.lat, ar.lng, ar.place)
	}
}

func TestExecuteForActivity_ResolvesPlace(t *testing.T) {
	ar := &fakeActivityRepo{}
	sr := &fakeStreamRepo{lat: 49.87, lon: 8.65, found: true}
	gc := &fakeGeocoder{place: port.Place{Name: "Darmstadt", Country: "Germany"}}
	uc := NewComputeStartPlace(ar, sr, gc, nil)

	if err := uc.ExecuteForActivity(context.Background(), cand()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ar.setCalled || ar.place != "Darmstadt" {
		t.Fatalf("expected place Darmstadt, got %q (called=%v)", ar.place, ar.setCalled)
	}
	if ar.lat == nil || ar.lng == nil || *ar.lat != 49.87 || *ar.lng != 8.65 {
		t.Fatalf("expected coords cached, got %v/%v", ar.lat, ar.lng)
	}
}

func TestExecuteForActivity_GeocoderError_LeavesNull(t *testing.T) {
	ar := &fakeActivityRepo{}
	sr := &fakeStreamRepo{lat: 1, lon: 2, found: true}
	gc := &fakeGeocoder{err: errors.New("nominatim 503")}
	uc := NewComputeStartPlace(ar, sr, gc, nil)

	err := uc.ExecuteForActivity(context.Background(), cand())
	if err == nil {
		t.Fatal("expected the transport error to propagate")
	}
	if ar.setCalled {
		t.Fatal("a transport error must NOT persist anything (so the row is retried)")
	}
}

func TestExecuteForActivity_NoPlace_MarksAttempted(t *testing.T) {
	ar := &fakeActivityRepo{}
	sr := &fakeStreamRepo{lat: 1, lon: 2, found: true}
	gc := &fakeGeocoder{place: port.Place{Country: "Atlantis"}, err: port.ErrNoPlace}
	uc := NewComputeStartPlace(ar, sr, gc, nil)

	if err := uc.ExecuteForActivity(context.Background(), cand()); err != nil {
		t.Fatalf("ErrNoPlace is a final answer, got %v", err)
	}
	if !ar.setCalled || ar.place != "" || ar.lat == nil || *ar.lat != 1 || ar.lng == nil || *ar.lng != 2 {
		t.Fatalf("no place should store coords and \"\", got (%v,%v,%q) called=%v", ar.lat, ar.lng, ar.place, ar.setCalled)
	}
}
