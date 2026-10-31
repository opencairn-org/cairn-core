package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
)

var tNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

type fedSpy struct{ created, deleted []domain.ActivityID }

func (f *fedSpy) PublishCreate(_ context.Context, a domain.Activity) {
	f.created = append(f.created, a.ID)
}
func (f *fedSpy) PublishDelete(_ context.Context, _ domain.UserID, id domain.ActivityID) {
	f.deleted = append(f.deleted, id)
}

// panicStreams panics on every aggregate refresh.
type panicStreams struct{ port.StreamRepo }

func (panicStreams) RefreshAggregates(context.Context, domain.SourceID) error { panic("boom") }

type fixture struct {
	p    *Pipeline
	repo *inmem.ActivityRepo
	fed  *fedSpy
	user domain.UserID
}

func newFixture() *fixture {
	f := &fixture{repo: inmem.NewActivityRepo(), fed: &fedSpy{}, user: domain.UserID(uuid.New())}
	recompute := activity.NewRecomputeActivityFromSources(f.repo, inmem.DefaultSettings{}, nil, nil, nil, nil, inmem.PassTx{}, func() time.Time { return tNow })
	f.p = &Pipeline{Activities: f.repo, Recompute: recompute, Federation: f.fed, Now: func() time.Time { return tNow }}
	return f
}

func (f *fixture) seed(sources int) (domain.ActivityID, []domain.SourceID) {
	aid := domain.ActivityID(uuid.New())
	f.repo.Activities[aid] = domain.Activity{ID: aid, UserID: f.user, Type: domain.ActivityTypeRide, StartTime: tNow, Privacy: domain.PrivacyPrivate}
	var ids []domain.SourceID
	for range sources {
		sid := domain.SourceID(uuid.New())
		f.repo.Sources[sid] = domain.ActivitySource{
			ID: sid, ActivityID: aid, UserID: f.user, Provider: "garmin", ExternalID: sid.String(), Status: domain.SourceStatusActive, ImportedAt: tNow,
			Parsed: domain.ActivitySourcePayload{Type: domain.ActivityTypeRide, StartTime: tNow, EndTime: tNow.Add(time.Hour), ElapsedDuration: time.Hour, MovingDuration: time.Hour, Timezone: "UTC"},
		}
		ids = append(ids, sid)
	}
	return aid, ids
}

func TestFollowSourceAddressesTheSurvivingActivity(t *testing.T) {
	f := newFixture()
	survivor, _ := f.seed(1)
	dissolved, srcs := f.seed(1)
	// Re-clustering moved the new source into the survivor.
	s := f.repo.Sources[srcs[0]]
	s.ActivityID = survivor
	f.repo.Sources[srcs[0]] = s

	out := f.p.followSource(context.Background(), activity.IngestResult{ActivityID: dissolved, SourceID: srcs[0], Action: activity.IngestActionImportedNew})
	if out.ActivityID != survivor || out.Activity.ID != survivor || out.Action != activity.IngestActionMergedIntoExisting {
		t.Fatalf("followSource: %+v", out)
	}
	// A reimport keeps its action; an unmoved source is untouched.
	stay, staySrc := f.seed(1)
	out = f.p.followSource(context.Background(), activity.IngestResult{ActivityID: stay, SourceID: staySrc[0], Action: activity.IngestActionReimportedExisting})
	if out.ActivityID != stay || out.Action != activity.IngestActionReimportedExisting {
		t.Fatalf("unmoved: %+v", out)
	}
}

func TestRemergeRetractsSoftDeleted(t *testing.T) {
	f := newFixture()
	aid, srcs := f.seed(1)
	s := f.repo.Sources[srcs[0]]
	s.Status = domain.SourceStatusDetached
	f.repo.Sources[srcs[0]] = s

	res, err := f.p.Remerge(context.Background(), aid)
	if err != nil || !res.SoftDeleted {
		t.Fatalf("remerge: %v %+v", err, res)
	}
	if len(f.fed.deleted) != 1 || f.fed.deleted[0] != aid {
		t.Fatalf("federation delete: %+v", f.fed.deleted)
	}
}

func TestRecomputeDerivedSurvivesPanickingStage(t *testing.T) {
	f := newFixture()
	aid, _ := f.seed(2)
	f.p.Streams = panicStreams{}
	n, warnings, err := f.p.RecomputeDerived(context.Background(), f.repo.Activities[aid])
	if err != nil || n != 2 || len(warnings) != 2 {
		t.Fatalf("recompute derived: %v n=%d %v", err, n, warnings)
	}
	// The event path reports the panic as a retryable error instead.
	err = f.p.RunFollowUps(context.Background(), ActivityChanged{Kind: ChangeBorrowed, UserID: f.user, ActivityID: aid, SourceID: domain.SourceID(uuid.New())})
	if !errors.Is(err, errPanic) {
		t.Fatalf("run follow-ups: %v", err)
	}
}

func TestBorrowFromValidates(t *testing.T) {
	f := newFixture()
	f.p.Tx, f.p.FieldOverrides = inmem.PassTx{}, nopOverrides{}
	target, _ := f.seed(1)
	donor, _ := f.seed(1)
	ctx := context.Background()
	cases := []struct {
		in   BorrowInput
		want error
	}{
		{BorrowInput{UserID: f.user, TargetID: target, DonorID: target, Groups: []domain.FieldGroup{domain.FieldGroupDistance}}, ErrBorrowSameActivity},
		{BorrowInput{UserID: f.user, TargetID: target, DonorID: donor}, ErrBorrowNoGroups},
		{BorrowInput{UserID: domain.UserID(uuid.New()), TargetID: target, DonorID: donor, Groups: []domain.FieldGroup{domain.FieldGroupDistance}}, domain.ErrNotFound},
		{BorrowInput{UserID: f.user, TargetID: target, DonorID: donor, Groups: []domain.FieldGroup{domain.FieldGroupGPSTrack}}, ErrBorrowNoRoute},
	}
	for i, c := range cases {
		if _, err := f.p.BorrowFrom(ctx, c.in); !errors.Is(err, c.want) {
			t.Errorf("case %d: %v, want %v", i, err, c.want)
		}
	}
	if _, err := f.p.BorrowFrom(ctx, BorrowInput{UserID: f.user, TargetID: target, DonorID: donor, Groups: []domain.FieldGroup{"title"}}); err == nil {
		t.Error("a non-borrowable group must be refused")
	}
}

func TestBorrowFromPinsDerivedSource(t *testing.T) {
	f := newFixture()
	pins := &recordingOverrides{}
	f.p.Tx, f.p.FieldOverrides = inmem.PassTx{}, pins
	target, _ := f.seed(1)
	donor, _ := f.seed(1)
	d := 42000.0
	a := f.repo.Activities[donor]
	a.Summary.DistanceM = &d
	f.repo.Activities[donor] = a

	sid, err := f.p.BorrowFrom(context.Background(), BorrowInput{UserID: f.user, TargetID: target, DonorID: donor, Groups: []domain.FieldGroup{domain.FieldGroupDistance}})
	if err != nil {
		t.Fatal(err)
	}
	src := f.repo.Sources[sid]
	if src.ActivityID != target || src.Provider != "derived" || src.Parsed.Summary.DistanceM == nil || *src.Parsed.Summary.DistanceM != d || src.Parsed.Summary.AvgHeartRateBpm != nil {
		t.Fatalf("derived source: %+v", src)
	}
	if len(pins.set) != 1 || pins.set[0].SourceID != sid || pins.set[0].FieldKey != domain.FieldGroupDistance {
		t.Fatalf("pins: %+v", pins.set)
	}
}

type nopOverrides struct{ port.FieldOverrideRepo }

func (nopOverrides) Set(context.Context, domain.FieldSourceOverride) error { return nil }
func (nopOverrides) ListForActivity(context.Context, domain.ActivityID) ([]domain.FieldSourceOverride, error) {
	return nil, nil
}

type recordingOverrides struct {
	port.FieldOverrideRepo
	set []domain.FieldSourceOverride
}

func (r *recordingOverrides) Set(_ context.Context, o domain.FieldSourceOverride) error {
	r.set = append(r.set, o)
	return nil
}
func (r *recordingOverrides) ListForActivity(context.Context, domain.ActivityID) ([]domain.FieldSourceOverride, error) {
	return r.set, nil
}
