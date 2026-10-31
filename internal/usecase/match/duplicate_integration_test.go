//go:build integration

package match

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/postgres"
	"github.com/opencairn-org/cairn-core/internal/db"
	"github.com/opencairn-org/cairn-core/internal/domain"
	dmatch "github.com/opencairn-org/cairn-core/internal/domain/match"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
)

// pgDuplicateFixture is a fresh user on the real schema with the re-cluster
// engine wired over the postgres repos.
type pgDuplicateFixture struct {
	ctx   context.Context
	repo  *postgres.ActivityRepo
	links *SourceLinks
	user  domain.UserID
}

func newPGDuplicateFixture(t *testing.T) pgDuplicateFixture {
	t.Helper()
	url := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CAIRN_TEST_DATABASE_URL unset — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Open(ctx, db.Config{URL: url, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	uid := domain.UserID(uuid.New())
	suffix := uid.String()[:8]
	if err := postgres.NewUserRepo(pool).CreateUser(ctx, domain.User{
		ID: uid, Username: "dup-" + suffix, Email: "dup-" + suffix + "@example.com", DisplayName: "Dup",
		Role: domain.UserRoleUser, Status: domain.UserStatusActive, Units: domain.UserUnitsMetric,
	}, domain.Credentials{UserID: uid}); err != nil {
		t.Fatal(err)
	}
	repo := postgres.NewActivityRepo(pool)
	tx := postgres.NewTxManager(pool)
	recompute := activity.NewRecomputeActivityFromSources(repo, fakeSettings{}, nil, nil, nil, nil, tx, time.Now)
	return pgDuplicateFixture{ctx: ctx, repo: repo, user: uid,
		links: NewSourceLinks(repo, NewReclusterBucket(repo, recompute, nil, tx, nil, dmatch.DefaultOptions(), nil))}
}

// seed stores a singleton activity with one strava source from the same pool.
func (f pgDuplicateFixture) seed(t *testing.T, start time.Time, distM float64) (domain.SourceID, domain.ActivityID) {
	t.Helper()
	aid, sid := domain.ActivityID(uuid.Must(uuid.NewV7())), domain.SourceID(uuid.Must(uuid.NewV7()))
	if err := f.repo.SaveActivity(f.ctx, domain.Activity{
		ID: aid, UserID: f.user, Type: domain.ActivityTypeRide, Title: "ride", StartTime: start, EndTime: start.Add(time.Hour),
		ElapsedDuration: time.Hour, MovingDuration: 55 * time.Minute, Timezone: "UTC", MergedAt: start, Privacy: domain.PrivacyPrivate,
	}); err != nil {
		t.Fatalf("save activity: %v", err)
	}
	d := distM
	if err := f.repo.SaveSource(f.ctx, domain.ActivitySource{
		ID: sid, ActivityID: aid, UserID: f.user, Provider: "strava", ExternalID: "ext-" + sid.String(),
		SourceWorkerName: "strava", SourceWorkerVersion: "1.0", SourceWorkerPackage: "p", Status: domain.SourceStatusActive,
		ReimportStatus: domain.ReimportStatusCurrent, ImportedAt: start, UpdatedAt: start,
		Parsed: domain.ActivitySourcePayload{
			Type: domain.ActivityTypeRide, Discipline: domain.DisciplineRideRoad, StartTime: start, EndTime: start.Add(time.Hour),
			ElapsedDuration: time.Hour, MovingDuration: 55 * time.Minute, Timezone: "UTC", Summary: domain.ActivitySummary{DistanceM: &d},
		},
	}); err != nil {
		t.Fatalf("save source: %v", err)
	}
	return sid, aid
}

func (f pgDuplicateFixture) recluster(t *testing.T, around time.Time) {
	t.Helper()
	if _, err := f.links.recluster.Execute(f.ctx, Input{UserID: f.user, Around: around}); err != nil {
		t.Fatalf("recluster: %v", err)
	}
}

func (f pgDuplicateFixture) queue(t *testing.T) []domain.ActivityReviewItem {
	t.Helper()
	items, err := f.repo.ListActivitiesNeedingReview(f.ctx, f.user)
	if err != nil {
		t.Fatalf("review queue: %v", err)
	}
	return items
}

// A Strava delete + re-upload: one review item, stable across re-clusters;
// resolving it as a merge leaves one live activity holding both sources.
func TestPossibleDuplicate_MergeIntegration(t *testing.T) {
	f := newPGDuplicateFixture(t)
	start := time.Date(2026, 5, 3, 7, 0, 0, 0, time.UTC)
	s1, a1 := f.seed(t, start, 30000)
	s2, a2 := f.seed(t, start.Add(2*time.Second), 30010)

	f.recluster(t, start)
	f.recluster(t, start)
	items := f.queue(t)
	if len(items) != 1 || items[0].Kind != domain.ReviewKindDuplicate || items[0].ID != a2 || items[0].DuplicateOf != a1 {
		t.Fatalf("queue %+v (a1 %s a2 %s)", items, a1, a2)
	}
	cons, err := f.repo.ListMatchConstraints(f.ctx, f.user)
	if err != nil || len(cons) != 1 || cons[0].Kind != domain.ConstraintSuspectedDuplicate {
		t.Fatalf("constraints %v %+v", err, cons)
	}

	if err := f.links.LinkActivities(f.ctx, f.user, items[0].DuplicateOf, items[0].ID); err != nil {
		t.Fatalf("merge: %v", err)
	}
	src1, _ := f.repo.GetSource(f.ctx, s1)
	src2, _ := f.repo.GetSource(f.ctx, s2)
	if src1.ActivityID != src2.ActivityID {
		t.Fatalf("sources not merged: %s vs %s", src1.ActivityID, src2.ActivityID)
	}
	live := 0
	for _, id := range []domain.ActivityID{a1, a2} {
		if a, err := f.repo.GetActivity(f.ctx, id); err == nil && !a.IsDeleted() {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("want 1 live activity, got %d", live)
	}
	if items := f.queue(t); len(items) != 0 {
		t.Fatalf("queue after merge %+v", items)
	}
}

// "Keep both" turns the suggestion into a cannot-link; a later suggestion for
// the pair never replaces that decision.
func TestPossibleDuplicate_KeepBothIntegration(t *testing.T) {
	f := newPGDuplicateFixture(t)
	start := time.Date(2026, 5, 4, 7, 0, 0, 0, time.UTC)
	s1, a1 := f.seed(t, start, 30000)
	s2, a2 := f.seed(t, start.Add(2*time.Second), 30010)
	f.recluster(t, start)
	if len(f.queue(t)) != 1 {
		t.Fatal("expected one review item")
	}

	if err := f.links.KeepApart(f.ctx, f.user, a2, a1); err != nil {
		t.Fatalf("keep apart: %v", err)
	}
	if err := f.repo.AddMatchConstraint(f.ctx, domain.MatchConstraint{
		UserID: f.user, SourceA: s1, SourceB: s2, Kind: domain.ConstraintSuspectedDuplicate, Reason: ReasonSamePoolConflict,
	}); err != nil {
		t.Fatal(err)
	}
	f.recluster(t, start)
	if items := f.queue(t); len(items) != 0 {
		t.Fatalf("queue after keep both %+v", items)
	}
	cons, err := f.repo.ListMatchConstraints(f.ctx, f.user)
	if err != nil || len(cons) != 1 || cons[0].Kind != domain.ConstraintCannotLink || cons[0].Reason != ReasonUserKeep {
		t.Fatalf("constraints %v %+v", err, cons)
	}
}
