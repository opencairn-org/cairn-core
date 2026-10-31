//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// seedGearActivity inserts a minimal valid activity attached to gear.
func seedGearActivity(t *testing.T, ctx context.Context, repo *ActivityRepo, userID domain.UserID, gear *domain.GearID, distanceM float64, movingS int) domain.ActivityID {
	t.Helper()
	start := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	a := domain.Activity{
		ID: domain.ActivityID(uuid.New()), UserID: userID, Type: domain.ActivityTypeRide,
		StartTime: start, EndTime: start.Add(2 * time.Hour), ElapsedDuration: 2 * time.Hour,
		MovingDuration: time.Duration(movingS) * time.Second, Timezone: "UTC",
		Summary: domain.ActivitySummary{DistanceM: &distanceM},
		Privacy: domain.PrivacyPrivate, GearID: gear, GearSetBy: domain.GearSetByMerge,
		MergeProvenance: domain.MergeProvenance{},
	}
	if err := repo.SaveActivity(ctx, a); err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	return a.ID
}

func TestGearRepo_Integration(t *testing.T) {
	ctx, pool := requirePool(t)
	userID := seedUser(t)
	gear := NewGearRepo(pool)
	activities := NewActivityRepo(pool)
	acctID, err := NewExternalAccountRepo(pool, 0, 0).CreateAccount(ctx, domain.ExternalAccount{
		UserID: userID, Provider: "strava", ProviderAccountID: "gtest-" + userID.String()[:8],
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	bike := domain.Gear{ID: domain.GearID(uuid.New()), UserID: userID, Kind: domain.GearKindBike, Name: "Canyon"}
	other := domain.Gear{ID: domain.GearID(uuid.New()), UserID: userID, Kind: domain.GearKindBike, Name: "Canyon (Garmin)"}
	for _, g := range []domain.Gear{bike, other} {
		if err := gear.Save(ctx, g); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	// Refs: a pending record first (no gear), then mapped; an upsert for the
	// same key repoints. The owner comes from the external account.
	key := domain.GearRefKey{Provider: "strava", ExternalAccountID: acctID, ExternalID: "b1"}
	missing := domain.GearRefKey{Provider: "strava", ExternalAccountID: acctID, ExternalID: "nope"}
	ref := domain.GearExternalRef{Provider: "strava", ExternalAccountID: acctID, ExternalID: "b1", ProviderName: "Canyon", Primary: true,
		Kind: domain.GearKindBike, Brand: "Canyon", DefaultFor: []domain.ActivityType{domain.ActivityTypeRide}}
	if err := gear.UpsertExternalRef(ctx, ref); err != nil {
		t.Fatalf("UpsertExternalRef (pending): %v", err)
	}
	if got, err := gear.GetExternalRef(ctx, key); err != nil || !got.Pending() || got.UserID != userID || got.Brand != "Canyon" || len(got.DefaultFor) != 1 {
		t.Fatalf("pending ref = %+v / %v", got, err)
	}
	if pending, err := gear.ListPendingRefs(ctx, userID); err != nil || len(pending) != 1 {
		t.Fatalf("ListPendingRefs = %d/%v", len(pending), err)
	}
	if resolved, err := gear.ResolveExternalRefs(ctx, []domain.GearRefKey{key}); err != nil || len(resolved) != 0 {
		t.Fatalf("pending ref must not resolve: %v/%v", resolved, err)
	}
	// MapPendingRef maps once; a second attempt reports it was not pending.
	if ok, err := gear.MapPendingRef(ctx, key, other.ID); err != nil || !ok {
		t.Fatalf("MapPendingRef = %v/%v", ok, err)
	}
	if ok, err := gear.MapPendingRef(ctx, key, bike.ID); err != nil || ok {
		t.Fatalf("MapPendingRef on mapped ref = %v/%v", ok, err)
	}
	if ok, _ := gear.MapPendingRef(ctx, missing, bike.ID); ok {
		t.Fatal("MapPendingRef on absent ref succeeded")
	}
	otherID0 := other.ID
	ref.GearID = &otherID0
	bikeID0 := bike.ID
	ref.GearID = &bikeID0
	if err := gear.UpsertExternalRef(ctx, ref); err != nil {
		t.Fatalf("UpsertExternalRef (repoint): %v", err)
	}
	if got, err := gear.GetExternalRef(ctx, key); err != nil || got.GearID == nil || *got.GearID != bike.ID {
		t.Fatalf("GetExternalRef = %+v/%v", got, err)
	}
	if pending, _ := gear.ListPendingRefs(ctx, userID); len(pending) != 0 {
		t.Fatalf("mapped ref listed as pending")
	}
	resolved, err := gear.ResolveExternalRefs(ctx, []domain.GearRefKey{key, missing})
	if err != nil || len(resolved) != 1 || resolved[key] != bike.ID {
		t.Fatalf("ResolveExternalRefs = %v/%v", resolved, err)
	}
	if g, _ := gear.Get(ctx, other.ID); len(g.ExternalRefs) != 0 {
		t.Fatalf("stale ref on other: %+v", g.ExternalRefs)
	}

	// Defaults.
	if err := gear.SetDefault(ctx, domain.GearDefault{UserID: userID, ActivityType: domain.ActivityTypeRide, GearID: other.ID}); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	if _, err := gear.GetDefault(ctx, userID, domain.ActivityTypeRun); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetDefault(run) = %v", err)
	}

	// Totals: live activities only, summed per gear; gear without activities zeroed.
	bikeID, otherID := bike.ID, other.ID
	a1 := seedGearActivity(t, ctx, activities, userID, &bikeID, 10000, 1800)
	seedGearActivity(t, ctx, activities, userID, &bikeID, 5000, 900)
	a3 := seedGearActivity(t, ctx, activities, userID, &otherID, 7000, 1000)
	if err := activities.SoftDeleteActivity(ctx, a3, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := gear.RecomputeTotalsForUser(ctx, userID); err != nil {
		t.Fatalf("RecomputeTotalsForUser: %v", err)
	}
	g, _ := gear.Get(ctx, bike.ID)
	if g.TotalDistanceM != 15000 || g.TotalMoving != 45*time.Minute || g.ActivityCount != 2 {
		t.Fatalf("bike totals = %v/%v/%v", g.TotalDistanceM, g.TotalMoving, g.ActivityCount)
	}
	if g, _ := gear.Get(ctx, other.ID); g.ActivityCount != 0 || g.TotalDistanceM != 0 {
		t.Fatalf("soft-deleted activity counted: %+v", g)
	}

	// gear_set_by round-trips; the gear filter finds attached activities.
	if a, err := activities.GetActivity(ctx, a1); err != nil || a.GearSetBy != domain.GearSetByMerge || a.GearID == nil || *a.GearID != bike.ID {
		t.Fatalf("activity gear round-trip: %+v %v", a, err)
	}
	if list, n, err := activities.ListActivitiesFiltered(ctx, userID, portGearFilter(bike.ID)); err != nil || n != 2 || len(list) != 2 {
		t.Fatalf("gear filter: n=%d len=%d err=%v", n, len(list), err)
	}

	// Activities to re-derive after a ref mapping change: merge-assigned
	// ones with a live source carrying the provider gear id.
	a2 := seedGearActivity(t, ctx, activities, userID, &bikeID, 1000, 100)
	userSet, _ := activities.GetActivity(ctx, a2)
	userSet.GearSetBy = domain.GearSetByUser
	if err := activities.SaveActivity(ctx, userSet); err != nil {
		t.Fatal(err)
	}
	for _, aid := range []domain.ActivityID{a1, a2} {
		src := domain.ActivitySource{
			ID: domain.SourceID(uuid.New()), ActivityID: aid, UserID: userID, Provider: "strava",
			ExternalAccountID: &acctID, ExternalID: "act-" + aid.String()[:8],
			Parsed: domain.ActivitySourcePayload{
				Type: domain.ActivityTypeRide, StartTime: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC),
				Timezone: "UTC", ElapsedDuration: time.Hour, GearExternalID: "b1",
			},
			Status: domain.SourceStatusActive, ReimportStatus: domain.ReimportStatusCurrent, ImportedAt: time.Now(),
		}
		if err := activities.SaveSource(ctx, src); err != nil {
			t.Fatalf("SaveSource: %v", err)
		}
	}
	if ids, err := gear.ListActivityIDsForRef(ctx, userID, key); err != nil || len(ids) != 1 || ids[0] != a1 {
		t.Fatalf("ListActivityIDsForRef = %v/%v (want only merge-set %s)", ids, err, a1)
	}
	if ids, _ := gear.ListActivityIDsForRef(ctx, domain.UserID(uuid.New()), key); len(ids) != 0 {
		t.Fatalf("other user's lookup returned %v", ids)
	}
	if ids, err := gear.ListActivityIDsForGear(ctx, bike.ID); err != nil || len(ids) != 3 {
		t.Fatalf("ListActivityIDsForGear = %v/%v", ids, err)
	}
	if names, err := gear.Names(ctx, []domain.GearID{bike.ID, other.ID, domain.GearID(uuid.New())}); err != nil || len(names) != 2 || names[bike.ID] != "Canyon" {
		t.Fatalf("Names = %v/%v", names, err)
	}

	// Repoint + delete = merge; the default and the activities follow.
	if err := gear.Repoint(ctx, other.ID, bike.ID); err != nil {
		t.Fatalf("Repoint: %v", err)
	}
	if err := gear.Delete(ctx, other.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if d, err := gear.GetDefault(ctx, userID, domain.ActivityTypeRide); err != nil || d != bike.ID {
		t.Fatalf("default after merge = %v/%v", d, err)
	}
	if a, _ := activities.GetActivity(ctx, a3); a.GearID == nil || *a.GearID != bike.ID {
		t.Fatalf("activity not repointed: %+v", a.GearID)
	}

	// The use case hands a user-set choice back to the merge, then deleting
	// the gear nulls the activities' gear (FK SET NULL).
	if err := gear.SetGearSetByForGear(ctx, bike.ID, domain.GearSetByMerge); err != nil {
		t.Fatalf("SetGearSetByForGear: %v", err)
	}
	if err := gear.Delete(ctx, bike.ID); err != nil {
		t.Fatalf("Delete bike: %v", err)
	}
	for _, aid := range []domain.ActivityID{a1, a2} {
		if a, _ := activities.GetActivity(ctx, aid); a.GearID != nil || a.GearSetBy != domain.GearSetByMerge {
			t.Fatalf("activity %s after delete: gear=%v set_by=%s", aid, a.GearID, a.GearSetBy)
		}
	}
	if err := gear.Delete(ctx, bike.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if list, err := gear.ListForUser(ctx, userID, true); err != nil || len(list) != 0 {
		t.Fatalf("ListForUser after deletes = %d/%v", len(list), err)
	}
}

func portGearFilter(id domain.GearID) port.ActivityListFilter {
	return port.ActivityListFilter{GearIDs: []domain.GearID{id}}
}
