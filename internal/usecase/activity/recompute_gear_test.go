package activity

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// memActivities is the slice of port.ActivityAggregateStore recompute touches.
type memActivities struct {
	port.ActivityAggregateStore
	activities map[domain.ActivityID]domain.Activity
	sources    map[domain.ActivityID][]domain.ActivitySource
}

func (m *memActivities) GetActivity(_ context.Context, id domain.ActivityID) (domain.Activity, error) {
	a, ok := m.activities[id]
	if !ok {
		return domain.Activity{}, domain.ErrNotFound
	}
	return a, nil
}
func (m *memActivities) ListSourcesForActivity(_ context.Context, id domain.ActivityID) ([]domain.ActivitySource, error) {
	return m.sources[id], nil
}
func (m *memActivities) SaveActivity(_ context.Context, a domain.Activity) error {
	m.activities[a.ID] = a
	return nil
}

type memSettings struct{ port.UserSettingsRepo }

func (memSettings) GetMergePolicy(_ context.Context, _ domain.UserID, t domain.ActivityType) (domain.MergePolicy, error) {
	return domain.DefaultMergePolicyFor(t), nil
}

type passTx struct{}

func (passTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

type gearFixture struct {
	uc         *RecomputeActivityFromSources
	acts       *memActivities
	gear       *inmem.GearRepo
	user       domain.UserID
	acct       domain.ExternalAccountID
	bike, shoe domain.GearID
}

func newGearFixture(t *testing.T) *gearFixture {
	t.Helper()
	f := &gearFixture{
		acts: &memActivities{activities: map[domain.ActivityID]domain.Activity{}, sources: map[domain.ActivityID][]domain.ActivitySource{}},
		gear: inmem.NewGearRepo(),
		user: domain.UserID(uuid.New()),
		acct: domain.ExternalAccountID(uuid.New()),
	}
	f.uc = NewRecomputeActivityFromSources(f.acts, memSettings{}, nil, nil, nil, f.gear, passTx{},
		func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) })
	f.bike, f.shoe = domain.GearID(uuid.New()), domain.GearID(uuid.New())
	for _, g := range []domain.Gear{
		{ID: f.bike, UserID: f.user, Kind: domain.GearKindBike, Name: "Bike"},
		{ID: f.shoe, UserID: f.user, Kind: domain.GearKindShoe, Name: "Shoe"},
	} {
		if err := f.gear.Save(context.Background(), g); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// source builds a valid ride source; gearExt "" = no provider gear id;
// provider "" = provider-less (manual).
func (f *gearFixture) source(aid domain.ActivityID, provider, gearExt string, importedAt time.Time) domain.ActivitySource {
	start := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	s := domain.ActivitySource{
		ID: domain.SourceID(uuid.New()), ActivityID: aid, UserID: f.user, Provider: provider,
		ExternalID: uuid.NewString(), Status: domain.SourceStatusActive, ImportedAt: importedAt,
		Parsed: domain.ActivitySourcePayload{
			Type: domain.ActivityTypeRide, StartTime: start, EndTime: start.Add(time.Hour),
			ElapsedDuration: time.Hour, MovingDuration: time.Hour, Timezone: "UTC", GearExternalID: gearExt,
		},
	}
	if provider == "" {
		s.Provider = "manual"
	} else {
		acct := f.acct
		s.ExternalAccountID = &acct
	}
	return s
}

// mapRef maps a provider gear id on the fixture account to gear.
func (f *gearFixture) mapRef(t *testing.T, provider, ext string, id domain.GearID) {
	t.Helper()
	if err := f.gear.UpsertExternalRef(context.Background(), domain.GearExternalRef{
		GearID: &id, UserID: f.user, Provider: provider, ExternalAccountID: f.acct, ExternalID: ext,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *gearFixture) run(t *testing.T, aid domain.ActivityID) domain.Activity {
	t.Helper()
	res, err := f.uc.Execute(context.Background(), aid)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	return res.Activity
}

func TestAssignGear_FromWinningSourceRef(t *testing.T) {
	f := newGearFixture(t)
	t0 := time.Now()
	aid := domain.ActivityID(uuid.New())
	f.mapRef(t, "strava", "b1", f.bike)
	f.acts.sources[aid] = []domain.ActivitySource{f.source(aid, "strava", "b1", t0)}

	a := f.run(t, aid)
	if a.GearID == nil || *a.GearID != f.bike || a.GearSetBy != domain.GearSetByMerge {
		t.Fatalf("gear = %v/%s", a.GearID, a.GearSetBy)
	}
	if w, ok := a.MergeProvenance.Winner(domain.FieldGroupGear); !ok || w != f.acts.sources[aid][0].ID {
		t.Fatalf("gear provenance = %v/%v", w, ok)
	}
	if f.gear.TotalsCalls != 1 {
		t.Fatalf("totals refreshed %d times, want 1", f.gear.TotalsCalls)
	}

	// Same sources again: nothing changed, no totals refresh.
	f.run(t, aid)
	if f.gear.TotalsCalls != 1 {
		t.Fatalf("unchanged recompute refreshed totals")
	}

	// Pending (unmapped) ref on a newer source: the mapped one still wins.
	f.acts.sources[aid] = append(f.acts.sources[aid], f.source(aid, "garmin", "77", t0.Add(time.Hour)))
	if a := f.run(t, aid); a.GearID == nil || *a.GearID != f.bike {
		t.Fatalf("unresolved newer source displaced the resolved one: %v", a.GearID)
	}
}

func TestAssignGear_UserChoiceSticks(t *testing.T) {
	f := newGearFixture(t)
	aid := domain.ActivityID(uuid.New())
	f.mapRef(t, "strava", "b1", f.bike)
	f.acts.sources[aid] = []domain.ActivitySource{f.source(aid, "strava", "b1", time.Now())}
	a := f.run(t, aid)

	// The user overrides Strava's bike with the shoe (nonsense, but theirs).
	a.GearID, a.GearSetBy = &f.shoe, domain.GearSetByUser
	f.acts.activities[aid] = a
	if a := f.run(t, aid); a.GearID == nil || *a.GearID != f.shoe || a.GearSetBy != domain.GearSetByUser {
		t.Fatalf("user choice overwritten: %v/%s", a.GearID, a.GearSetBy)
	}
	// Explicit "no gear" sticks too.
	a.GearID = nil
	f.acts.activities[aid] = a
	if a := f.run(t, aid); a.GearID != nil {
		t.Fatalf("user 'no gear' overwritten: %v", a.GearID)
	}
}

func TestAssignGear_SourceGearDroppedWhenSourceGone(t *testing.T) {
	f := newGearFixture(t)
	aid := domain.ActivityID(uuid.New())
	f.mapRef(t, "strava", "b1", f.bike)
	f.acts.sources[aid] = []domain.ActivitySource{f.source(aid, "strava", "b1", time.Now())}
	f.run(t, aid)

	// The provider record is unmapped again (e.g. the gear was deleted and
	// re-imported pending): gear that came from a source goes away.
	if err := f.gear.UpsertExternalRef(context.Background(), domain.GearExternalRef{UserID: f.user, Provider: "strava", ExternalAccountID: f.acct, ExternalID: "b1"}); err != nil {
		t.Fatal(err)
	}
	if a := f.run(t, aid); a.GearID != nil {
		t.Fatalf("source-derived gear kept after its ref unmapped: %v", a.GearID)
	}
}

func TestAssignGear_DefaultForProviderlessOnce(t *testing.T) {
	f := newGearFixture(t)
	ctx := context.Background()
	_ = f.gear.SetDefault(ctx, domain.GearDefault{UserID: f.user, ActivityType: domain.ActivityTypeRide, GearID: f.bike})
	aid := domain.ActivityID(uuid.New())
	f.acts.sources[aid] = []domain.ActivitySource{f.source(aid, "", "", time.Now())}

	a := f.run(t, aid)
	if a.GearID == nil || *a.GearID != f.bike || a.GearSetBy != domain.GearSetByMerge {
		t.Fatalf("first merge of a manual activity should take the default: %v/%s", a.GearID, a.GearSetBy)
	}
	if _, ok := a.MergeProvenance.Winner(domain.FieldGroupGear); ok {
		t.Fatal("a default must not record gear provenance")
	}
	// Default changes later: history is not rewritten, the gear stays.
	_ = f.gear.SetDefault(ctx, domain.GearDefault{UserID: f.user, ActivityType: domain.ActivityTypeRide, GearID: f.shoe})
	if a := f.run(t, aid); a.GearID == nil || *a.GearID != f.bike {
		t.Fatalf("default change rewrote history: %v", a.GearID)
	}

	// A provider import never takes the default.
	bid := domain.ActivityID(uuid.New())
	f.acts.sources[bid] = []domain.ActivitySource{f.source(bid, "strava", "", time.Now())}
	if a := f.run(t, bid); a.GearID != nil {
		t.Fatalf("provider activity took the type default: %v", a.GearID)
	}
}

func TestAssignGear_PinWins(t *testing.T) {
	f := newGearFixture(t)
	t0 := time.Now()
	aid := domain.ActivityID(uuid.New())
	f.mapRef(t, "strava", "b1", f.bike)
	f.mapRef(t, "garmin", "77", f.shoe)
	older := f.source(aid, "strava", "b1", t0)
	newer := f.source(aid, "garmin", "77", t0.Add(time.Hour))
	f.acts.sources[aid] = []domain.ActivitySource{older, newer}
	if a := f.run(t, aid); a.GearID == nil || *a.GearID != f.shoe {
		t.Fatalf("default policy should take the most recent source: %v", a.GearID)
	}
	f.uc.overrides = pinnedGear{aid: older.ID}
	if a := f.run(t, aid); a.GearID == nil || *a.GearID != f.bike {
		t.Fatalf("gear pin ignored: %v", a.GearID)
	}
}

// pinnedGear is a FieldOverrideRepo with one gear pin per activity.
type pinnedGear map[domain.ActivityID]domain.SourceID

func (p pinnedGear) ListForActivity(_ context.Context, id domain.ActivityID) ([]domain.FieldSourceOverride, error) {
	if sid, ok := p[id]; ok {
		return []domain.FieldSourceOverride{{ActivityID: id, FieldKey: domain.FieldGroupGear, SourceID: sid}}, nil
	}
	return nil, nil
}
func (pinnedGear) Set(context.Context, domain.FieldSourceOverride) error { return nil }
func (pinnedGear) Delete(context.Context, domain.ActivityID, domain.FieldGroup) error {
	return nil
}
