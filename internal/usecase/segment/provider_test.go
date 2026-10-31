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

type fakeProviderStore struct {
	providerSegmentStore
	byExternal map[string]domain.SegmentID
	saved      []domain.Segment
	efforts    []domain.SegmentEffort
	ranked     []domain.SegmentID
	attach     bool
	attached   []string
	finds      int
}

func (f *fakeProviderStore) FindSegmentIDByExternal(_ context.Context, _ domain.ExternalAccountID, ext string) (domain.SegmentID, error) {
	f.finds++
	if id, ok := f.byExternal[ext]; ok {
		return id, nil
	}
	return domain.SegmentID{}, domain.ErrNotFound
}

func (f *fakeProviderStore) SaveSegment(_ context.Context, s domain.Segment) error {
	f.saved = append(f.saved, s)
	f.byExternal[*s.ExternalID] = s.ID
	return nil
}

func (f *fakeProviderStore) SaveSegmentEffort(_ context.Context, e domain.SegmentEffort) error {
	f.efforts = append(f.efforts, e)
	return nil
}

func (f *fakeProviderStore) RecomputeRanksForSegment(_ context.Context, id domain.SegmentID) error {
	f.ranked = append(f.ranked, id)
	return nil
}

func (f *fakeProviderStore) AttachProviderEffortRef(_ context.Context, _ domain.SegmentID, _ domain.SourceID, ext string, _ time.Time, tol int) (bool, error) {
	if tol != providerEffortAttachToleranceS {
		return false, errors.New("tolerance")
	}
	f.attached = append(f.attached, ext)
	return f.attach, nil
}

type fakeSources struct {
	port.SourceReader
	src domain.ActivitySource
}

func (f *fakeSources) FindSourceByExternalID(_ context.Context, provider string, acc *domain.ExternalAccountID, ext string) (domain.ActivitySource, error) {
	if provider != f.src.Provider || acc == nil || *acc != *f.src.ExternalAccountID || ext != f.src.ExternalID {
		return domain.ActivitySource{}, domain.ErrNotFound
	}
	return f.src, nil
}

func providerFixture(gps bool) (*ProviderSegments, *fakeProviderStore, domain.ActivitySource) {
	acc := domain.ExternalAccountID(uuid.New())
	src := domain.ActivitySource{
		ID: domain.SourceID(uuid.New()), ActivityID: domain.ActivityID(uuid.New()), UserID: domain.UserID(uuid.New()),
		Provider: "strava", ExternalID: "a1", ExternalAccountID: &acc,
	}
	if gps {
		lat := 47.0
		src.Parsed.HasStream, src.StartLat = true, &lat
	}
	store := &fakeProviderStore{byExternal: map[string]domain.SegmentID{}}
	return NewProviderSegments(store, &fakeSources{src: src}), store, src
}

func externalSegment(acc domain.ExternalAccountID, ext string) domain.Segment {
	return domain.Segment{
		Source: domain.SegmentSourceExternal, ExternalID: &ext, ExternalAccountID: &acc, Scope: domain.SegmentScopePrivate,
		Name: "climb", ActivityType: domain.ActivityTypeRide, Polyline: "_p~iF~ps|U_ulLnnqC", PolylinePrecision: 5, DistanceM: 1000,
	}
}

func TestProviderSegments_UpsertIsIdempotent(t *testing.T) {
	uc, store, src := providerFixture(false)
	ctx := t.Context()
	first, err := uc.Batch().Upsert(ctx, externalSegment(*src.ExternalAccountID, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := uc.Batch().Upsert(ctx, externalSegment(*src.ExternalAccountID, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	if first != again || len(store.saved) != 2 || store.saved[1].ID != first {
		t.Fatalf("re-upsert must reuse the id: %v %v", first, again)
	}
}

func TestProviderSegments_UpsertRejects(t *testing.T) {
	uc, store, src := providerFixture(false)
	ctx := t.Context()
	noExt := externalSegment(*src.ExternalAccountID, "")
	if _, err := uc.Batch().Upsert(ctx, noExt); err == nil {
		t.Fatal("missing external id must fail")
	}
	bad := externalSegment(*src.ExternalAccountID, "s1")
	bad.Polyline = ""
	if _, err := uc.Batch().Upsert(ctx, bad); err == nil {
		t.Fatal("invalid segment must fail")
	}
	if len(store.saved) != 0 {
		t.Fatal("rejected segment must not persist")
	}
}

func TestProviderSegments_StreamlessStoresEffortAndRanks(t *testing.T) {
	uc, store, src := providerFixture(false)
	ctx := t.Context()
	b := uc.Batch()
	segID, err := b.Upsert(ctx, externalSegment(*src.ExternalAccountID, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	finds := store.finds
	res, err := b.RecordEffort(ctx, ProviderEffort{
		Provider: "strava", AccountID: *src.ExternalAccountID, SegmentExternalID: "s1", ActivityExternalID: "a1",
		ProviderEffortID: "e1", Effort: domain.SegmentEffort{ElapsedS: 90},
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.finds != finds {
		t.Fatal("segment upserted in the batch must resolve without a lookup")
	}
	if res.Outcome != EffortStored || len(store.efforts) != 1 || len(store.ranked) != 1 || store.ranked[0] != segID {
		t.Fatalf("result %+v efforts %d ranked %v", res, len(store.efforts), store.ranked)
	}
	e := store.efforts[0]
	if e.SegmentID != segID || e.ActivityID != src.ActivityID || e.ActivitySourceID != src.ID || e.UserID != src.UserID || e.ID == (domain.SegmentEffortID{}) || e.ElapsedS != 90 {
		t.Fatalf("effort %+v", e)
	}
}

func TestProviderSegments_GPSSourceOnlyEnrichesMatcher(t *testing.T) {
	for _, attach := range []bool{true, false} {
		uc, store, src := providerFixture(true)
		store.attach = attach
		store.byExternal["s1"] = domain.SegmentID(uuid.New())
		res, err := uc.Batch().RecordEffort(t.Context(), ProviderEffort{
			Provider: "strava", AccountID: *src.ExternalAccountID, SegmentExternalID: "s1", ActivityExternalID: "a1", ProviderEffortID: "e1",
		})
		if err != nil {
			t.Fatal(err)
		}
		want := EffortDropped
		if attach {
			want = EffortAttached
		}
		if res.Outcome != want || len(store.attached) != 1 || len(store.efforts) != 0 || len(store.ranked) != 0 {
			t.Fatalf("attach=%v: result %+v", attach, res)
		}
	}
}

func TestProviderSegments_EffortErrors(t *testing.T) {
	uc, store, src := providerFixture(false)
	ctx := t.Context()
	if _, err := uc.Batch().RecordEffort(ctx, ProviderEffort{Provider: "strava", AccountID: *src.ExternalAccountID, SegmentExternalID: "nope", ActivityExternalID: "a1"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown segment: %v", err)
	}
	store.byExternal["s1"] = domain.SegmentID(uuid.New())
	if _, err := uc.Batch().RecordEffort(ctx, ProviderEffort{Provider: "strava", AccountID: *src.ExternalAccountID, SegmentExternalID: "s1", ActivityExternalID: "other"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown activity: %v", err)
	}
	if len(store.efforts) != 0 {
		t.Fatal("failed effort must not persist")
	}
}
