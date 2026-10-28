package match

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	dmatch "github.com/opencairn-org/cairn-core/internal/domain/match"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
)

// seedSource adds a singleton activity with one active source, as ingest does.
func seedSource(r *inmem.ActivityRepo, user domain.UserID, provider string, start time.Time, distM float64) (domain.SourceID, domain.ActivityID) {
	sid := domain.SourceID(uuid.New())
	aid := domain.ActivityID(uuid.New())
	d := distM
	r.Sources[sid] = domain.ActivitySource{
		ID: sid, ActivityID: aid, UserID: user, Provider: provider, ExternalID: provider + "-" + sid.String(),
		Status: domain.SourceStatusActive, ImportedAt: reTime,
		Parsed: domain.ActivitySourcePayload{
			Type: domain.ActivityTypeRide, Discipline: domain.DisciplineRideRoad, StartTime: start, EndTime: start.Add(time.Hour),
			ElapsedDuration: time.Hour, MovingDuration: 55 * time.Minute, Timezone: "UTC",
			Summary: domain.ActivitySummary{DistanceM: &d},
		},
	}
	r.Activities[aid] = domain.Activity{ID: aid, UserID: user, Type: domain.ActivityTypeRide, StartTime: start, Privacy: domain.PrivacyPrivate}
	return sid, aid
}

func newLinks(r *inmem.ActivityRepo) *SourceLinks {
	recompute := activity.NewRecomputeActivityFromSources(r, inmem.DefaultSettings{}, nil, nil, nil, nil, inmem.PassTx{}, func() time.Time { return reTime })
	return NewSourceLinks(r, NewReclusterBucket(r, recompute, nil, inmem.PassTx{}, nil, dmatch.DefaultOptions(), nil))
}

// Two rides the matcher keeps apart (3h gap) are joined by a must-link.
func TestSourceLinks_LinkJoinsDistinctActivities(t *testing.T) {
	r := inmem.NewActivityRepo()
	s1, a1 := seedSource(r, testUser, "garmin", reTime, 30000)
	s2, a2 := seedSource(r, testUser, "strava", reTime.Add(3*time.Hour), 25000)

	got, err := newLinks(r).Link(t.Context(), testUser, s2, a1)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if r.Sources[s1].ActivityID != r.Sources[s2].ActivityID {
		t.Fatalf("sources not joined: %s vs %s", r.Sources[s1].ActivityID, r.Sources[s2].ActivityID)
	}
	if got.ID != r.Sources[s2].ActivityID || got.DeletedAt != nil {
		t.Fatalf("returned activity %s is not the live home of the source", got.ID)
	}
	if a := r.Activities[a1]; a.DeletedAt != nil && r.Activities[a2].DeletedAt != nil {
		t.Fatal("exactly one of the originals must survive")
	}
	if len(r.Constraints) != 1 || r.Constraints[0].Kind != domain.ConstraintMustLink || r.Constraints[0].Reason != ReasonUserMerge {
		t.Fatalf("constraint %+v", r.Constraints)
	}
}

func TestSourceLinks_LinkRefusals(t *testing.T) {
	r := inmem.NewActivityRepo()
	s1, a1 := seedSource(r, testUser, "garmin", reTime, 30000)
	other := domain.UserID(uuid.New())
	_, aOther := seedSource(r, other, "garmin", reTime, 30000)
	uc := newLinks(r)
	if _, err := uc.Link(t.Context(), testUser, s1, a1); !errors.Is(err, ErrSameActivity) {
		t.Errorf("same activity: %v", err)
	}
	if _, err := uc.Link(t.Context(), testUser, s1, aOther); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign target: %v", err)
	}
	if _, err := uc.Link(t.Context(), other, s1, aOther); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign source: %v", err)
	}
	if len(r.Constraints) != 0 {
		t.Fatal("refusals must not record constraints")
	}
}

// A duplicate pair the matcher merged is split back into two activities.
func TestSourceLinks_SplitPeelsSource(t *testing.T) {
	r := inmem.NewActivityRepo()
	s1, _ := seedSource(r, testUser, "garmin", reTime, 30050)
	s2, _ := seedSource(r, testUser, "strava", reTime.Add(2*time.Second), 30000)
	uc := newLinks(r)
	if _, err := uc.recluster.Execute(t.Context(), Input{UserID: testUser, Around: reTime}); err != nil {
		t.Fatalf("seed recluster: %v", err)
	}
	merged := r.Sources[s1].ActivityID
	if merged != r.Sources[s2].ActivityID {
		t.Fatal("precondition: sources should be merged")
	}

	got, err := uc.Split(t.Context(), testUser, merged, s2)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if r.Sources[s1].ActivityID == r.Sources[s2].ActivityID {
		t.Fatal("sources still share an activity")
	}
	if got.ID != r.Sources[s2].ActivityID || got.DeletedAt != nil {
		t.Fatalf("returned %s, source now lives in %s", got.ID, r.Sources[s2].ActivityID)
	}
	if len(r.Constraints) != 1 || r.Constraints[0].Kind != domain.ConstraintCannotLink {
		t.Fatalf("constraint %+v", r.Constraints)
	}
}

func TestSourceLinks_SplitRefusals(t *testing.T) {
	r := inmem.NewActivityRepo()
	s1, a1 := seedSource(r, testUser, "garmin", reTime, 30000)
	_, a2 := seedSource(r, testUser, "garmin", reTime.Add(3*time.Hour), 30000)
	uc := newLinks(r)
	if _, err := uc.Split(t.Context(), testUser, a1, s1); !errors.Is(err, ErrSingleSource) {
		t.Errorf("single source: %v", err)
	}
	if _, err := uc.Split(t.Context(), testUser, a2, s1); !errors.Is(err, ErrSourceNotOnActivity) {
		t.Errorf("wrong pair: %v", err)
	}
	if _, err := uc.Split(t.Context(), domain.UserID(uuid.New()), a1, s1); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign: %v", err)
	}
}

func TestSourceLinks_LinkActivities(t *testing.T) {
	r := inmem.NewActivityRepo()
	s1, a1 := seedSource(r, testUser, "garmin", reTime, 30000)
	s2, a2 := seedSource(r, testUser, "strava", reTime.Add(3*time.Hour), 25000)
	uc := newLinks(r)
	if err := uc.LinkActivities(t.Context(), testUser, a1, a1); !errors.Is(err, ErrSameActivity) {
		t.Errorf("same: %v", err)
	}
	if err := uc.LinkActivities(t.Context(), testUser, a1, a2); err != nil {
		t.Fatalf("link: %v", err)
	}
	if r.Sources[s1].ActivityID != r.Sources[s2].ActivityID {
		t.Fatal("not joined")
	}
}
