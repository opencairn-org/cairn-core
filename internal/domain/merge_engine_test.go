package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// fixedNow returns a deterministic timestamp used across all tests so
// produced MergedAt values are reproducible.
var fixedNow = time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

// makeUserID and makeSourceID generate deterministic UUIDs from a string
// seed so tests can assert specific ID values in the provenance map.
func makeUserID(seed string) UserID {
	return UserID(uuid.NewSHA1(uuid.NameSpaceOID, []byte("user:"+seed)))
}

func makeSourceID(seed string) SourceID {
	return SourceID(uuid.NewSHA1(uuid.NameSpaceOID, []byte("source:"+seed)))
}

func makeActivityID(seed string) ActivityID {
	return ActivityID(uuid.NewSHA1(uuid.NameSpaceOID, []byte("activity:"+seed)))
}

// rideStart is the shared activity start time used across cases.
var rideStart = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

// makeStravaRide returns a fully-populated Strava ride source. importOffset
// shifts the ImportedAt so callers can stage same-source/different-import
// scenarios.
func makeStravaRide(idSeed string, userID UserID, activityID ActivityID, importOffset time.Duration) ActivitySource {
	return ActivitySource{
		ID:         makeSourceID(idSeed),
		ActivityID: activityID,
		UserID:     userID,
		Provider:   "strava",
		ExternalID: "1001-" + idSeed,
		Parsed: ActivitySourcePayload{
			Type:            ActivityTypeRide,
			Discipline:      DisciplineRideRoad,
			Title:           "Strava Lunch Ride",
			Description:     "Felt good",
			StartTime:       rideStart,
			EndTime:         rideStart.Add(1 * time.Hour),
			ElapsedDuration: 1 * time.Hour,
			MovingDuration:  55 * time.Minute,
			Timezone:        "Europe/Berlin",
			Summary: ActivitySummary{
				DistanceM:       Ptr(30000.0),
				ElevationGainM:  Ptr(250.0),
				AvgSpeedMps:     Ptr(8.3),
				AvgHeartRateBpm: Ptr(145),
				MaxHeartRateBpm: Ptr(178),
				AvgPowerW:       Ptr(180), // Strava power is notoriously fuzzy
				MaxPowerW:       Ptr(420),
				CaloriesKcal:    Ptr(750),
			},
			HasStream: true,
		},
		ImportedAt: fixedNow.Add(importOffset),
	}
}

// makeGarminRide returns a Garmin ride source with high-fidelity power data.
func makeGarminRide(idSeed string, userID UserID, activityID ActivityID, importOffset time.Duration) ActivitySource {
	dist := 30050.0 // slight disagreement with Strava — Garmin's GPS is more granular
	return ActivitySource{
		ID:         makeSourceID(idSeed),
		ActivityID: activityID,
		UserID:     userID,
		Provider:   "garmin",
		ExternalID: "2001-" + idSeed,
		Parsed: ActivitySourcePayload{
			Type:            ActivityTypeRide,
			Discipline:      DisciplineRideRoad,
			Title:           "Cycling",
			Description:     "",
			StartTime:       rideStart,
			EndTime:         rideStart.Add(1 * time.Hour),
			ElapsedDuration: 1 * time.Hour,
			MovingDuration:  56 * time.Minute,
			Timezone:        "Europe/Berlin",
			Summary: ActivitySummary{
				DistanceM:        Ptr(dist),
				ElevationGainM:   Ptr(248.0),
				AvgSpeedMps:      Ptr(8.4),
				AvgHeartRateBpm:  Ptr(146),
				MaxHeartRateBpm:  Ptr(179),
				AvgPowerW:        Ptr(195), // truer power from a real meter
				MaxPowerW:        Ptr(450),
				NormalizedPowerW: Ptr(210),
				AvgCadence:       Ptr(85),
				MaxCadence:       Ptr(110),
				AvgTemperatureC:  Ptr(18.5),
				CaloriesKcal:     Ptr(680), // Garmin uses HR+power; Strava uses estimation
				TSS:              Ptr(72.0),
				IntensityFactor:  Ptr(0.83),
			},
			HasStream: true,
		},
		ImportedAt: fixedNow.Add(importOffset),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestMerge_RideGarminWinsPower_StravaWinsTitle(t *testing.T) {
	user := makeUserID("alice")
	actID := makeActivityID("ride1")

	// Strava imported 1 hour earlier than Garmin (recency does NOT decide
	// the per-field-group winner — the policy does).
	strava := makeStravaRide("s1", user, actID, -1*time.Hour)
	garmin := makeGarminRide("g1", user, actID, 0)

	resolver := func(t ActivityType) MergePolicy {
		return testMergePolicyFor(t)
	}

	merged, err := Merge(actID, []ActivitySource{strava, garmin}, resolver, fixedNow)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Default ride policy: classification + title + description → Strava;
	// everything else → Garmin > Strava > _any.
	assertSourceWon(t, merged, FieldGroupClassification, strava.ID)
	assertSourceWon(t, merged, FieldGroupTitle, strava.ID)
	assertSourceWon(t, merged, FieldGroupDescription, strava.ID)

	assertSourceWon(t, merged, FieldGroupDistance, garmin.ID)
	assertSourceWon(t, merged, FieldGroupPower, garmin.ID)
	assertSourceWon(t, merged, FieldGroupHeartRate, garmin.ID)
	assertSourceWon(t, merged, FieldGroupCadence, garmin.ID)
	assertSourceWon(t, merged, FieldGroupCalories, garmin.ID)

	// Verify the actual field values were lifted from the winners.
	if merged.Title != "Strava Lunch Ride" {
		t.Errorf("Title = %q, want %q", merged.Title, "Strava Lunch Ride")
	}
	if got := Deref(merged.Summary.AvgPowerW); got != 195 {
		t.Errorf("AvgPowerW = %d, want 195 (Garmin)", got)
	}
	if got := Deref(merged.Summary.CaloriesKcal); got != 680 {
		t.Errorf("CaloriesKcal = %d, want 680 (Garmin)", got)
	}

	// Stream-bearing source becomes the primary. Both have HasStream=true;
	// Garmin wins the ride default policy.
	if merged.PrimaryStreamSourceID == nil {
		t.Fatal("PrimaryStreamSourceID is nil")
	}
	if *merged.PrimaryStreamSourceID != garmin.ID {
		t.Errorf("PrimaryStreamSourceID = %s, want garmin %s",
			merged.PrimaryStreamSourceID, garmin.ID)
	}
}

func TestMerge_AnyProvider_PicksUnnamedSource(t *testing.T) {
	// Three sources: Strava, Garmin, Polar. Policy default = [garmin, _any].
	// Strava provides power and Polar provides power. _any should match
	// the most-recently-imported non-garmin source.
	user := makeUserID("alice")
	actID := makeActivityID("ride2")

	garmin := makeGarminRide("g1", user, actID, -2*time.Hour)
	// Garmin's Power is nilled out — simulates Garmin source w/o power meter.
	garmin.Parsed.Summary.AvgPowerW = nil
	garmin.Parsed.Summary.MaxPowerW = nil
	garmin.Parsed.Summary.NormalizedPowerW = nil

	strava := makeStravaRide("s1", user, actID, -1*time.Hour)
	polar := ActivitySource{
		ID: makeSourceID("p1"), ActivityID: actID, UserID: user,
		Provider: "polar", ExternalID: "3001-p1",
		Parsed: ActivitySourcePayload{
			Type: ActivityTypeRide, Discipline: DisciplineRideRoad,
			StartTime: rideStart, EndTime: rideStart.Add(1 * time.Hour),
			ElapsedDuration: 1 * time.Hour, MovingDuration: 56 * time.Minute,
			Timezone: "Europe/Berlin",
			Summary: ActivitySummary{
				AvgPowerW: Ptr(190),
				MaxPowerW: Ptr(410),
			},
		},
		ImportedAt: fixedNow,
	}

	resolver := func(t ActivityType) MergePolicy {
		return MergePolicy{
			DefaultPriority: []string{"garmin", AnyProvider},
		}
	}

	merged, err := Merge(actID, []ActivitySource{strava, garmin, polar}, resolver, fixedNow)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Garmin had no power; AnyProvider matches non-garmin sources in
	// recency order (Polar > Strava because importedAt).
	assertSourceWon(t, merged, FieldGroupPower, polar.ID)
	if got := Deref(merged.Summary.AvgPowerW); got != 190 {
		t.Errorf("AvgPowerW = %d, want 190 (Polar)", got)
	}
}

func TestMerge_Idempotent(t *testing.T) {
	// Same input ⇒ same output, byte-for-byte (modulo map iteration order
	// in tests we compare via direct lookup, which is order-independent).
	user := makeUserID("alice")
	actID := makeActivityID("ride3")
	strava := makeStravaRide("s1", user, actID, -1*time.Hour)
	garmin := makeGarminRide("g1", user, actID, 0)
	resolver := func(t ActivityType) MergePolicy { return testMergePolicyFor(t) }

	first, err := Merge(actID, []ActivitySource{strava, garmin}, resolver, fixedNow)
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	second, err := Merge(actID, []ActivitySource{strava, garmin}, resolver, fixedNow)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}

	// Compare every field-group winner; the activity field bodies follow
	// from the winners.
	for _, g := range AllFieldGroups {
		a, aOK := first.MergeProvenance.Winner(g)
		b, bOK := second.MergeProvenance.Winner(g)
		if aOK != bOK || a != b {
			t.Errorf("group %s differs between runs: %v vs %v", g, a, b)
		}
	}

	// PrimaryStreamSourceID stable.
	if (first.PrimaryStreamSourceID == nil) != (second.PrimaryStreamSourceID == nil) {
		t.Errorf("PrimaryStreamSourceID presence differs between runs")
	} else if first.PrimaryStreamSourceID != nil &&
		*first.PrimaryStreamSourceID != *second.PrimaryStreamSourceID {
		t.Errorf("PrimaryStreamSourceID differs: %v vs %v",
			first.PrimaryStreamSourceID, second.PrimaryStreamSourceID)
	}
}

func TestMerge_InputOrderIndependent(t *testing.T) {
	// Reordering the input slice must not change the merged result.
	// The engine sorts by ImportedAt internally.
	user := makeUserID("alice")
	actID := makeActivityID("ride4")
	strava := makeStravaRide("s1", user, actID, -1*time.Hour)
	garmin := makeGarminRide("g1", user, actID, 0)
	resolver := func(t ActivityType) MergePolicy { return testMergePolicyFor(t) }

	a, err := Merge(actID, []ActivitySource{strava, garmin}, resolver, fixedNow)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := Merge(actID, []ActivitySource{garmin, strava}, resolver, fixedNow)
	if err != nil {
		t.Fatalf("b: %v", err)
	}

	for _, g := range AllFieldGroups {
		wA, _ := a.MergeProvenance.Winner(g)
		wB, _ := b.MergeProvenance.Winner(g)
		if wA != wB {
			t.Errorf("group %s differs under reorder: %v vs %v", g, wA, wB)
		}
	}
}

func TestMerge_ErrEmptySources(t *testing.T) {
	resolver := func(t ActivityType) MergePolicy { return testMergePolicyFor(t) }
	_, err := Merge(makeActivityID("none"), nil, resolver, fixedNow)
	if !errors.Is(err, ErrNoSources) {
		t.Errorf("err = %v, want ErrNoSources", err)
	}
}

func TestMerge_ErrMixedUsers(t *testing.T) {
	actID := makeActivityID("ride5")
	a := makeStravaRide("s1", makeUserID("alice"), actID, 0)
	b := makeStravaRide("s2", makeUserID("bob"), actID, 0)
	resolver := func(t ActivityType) MergePolicy { return testMergePolicyFor(t) }
	_, err := Merge(actID, []ActivitySource{a, b}, resolver, fixedNow)
	if !errors.Is(err, ErrSourcesMismatchedUser) {
		t.Errorf("err = %v, want ErrSourcesMismatchedUser", err)
	}
}

func TestMerge_ErrNoClassification(t *testing.T) {
	// Source with empty Type (unparseable / worker bug). Provides()
	// reports false for Classification → merge engine returns ErrNoClassification.
	user := makeUserID("alice")
	actID := makeActivityID("ride6")
	bad := ActivitySource{
		ID: makeSourceID("bad"), ActivityID: actID, UserID: user,
		Provider: "fauxprovider",
		Parsed: ActivitySourcePayload{
			Type:            "", // empty — Provides(Classification) returns false
			StartTime:       rideStart,
			ElapsedDuration: 1 * time.Hour,
		},
		ImportedAt: fixedNow,
	}
	resolver := func(t ActivityType) MergePolicy { return testMergePolicyFor(t) }
	_, err := Merge(actID, []ActivitySource{bad}, resolver, fixedNow)
	if !errors.Is(err, ErrNoClassification) {
		t.Errorf("err = %v, want ErrNoClassification", err)
	}
}

func TestMerge_DerivesEndTimeWhenMissing(t *testing.T) {
	// A source that reports StartTime + ElapsedDuration but no EndTime
	// (set EndTime to zero) — the merge engine should derive it.
	user := makeUserID("alice")
	actID := makeActivityID("ride7")
	src := makeGarminRide("g1", user, actID, 0)
	src.Parsed.EndTime = time.Time{} // not provided

	resolver := func(t ActivityType) MergePolicy { return testMergePolicyFor(t) }
	merged, err := Merge(actID, []ActivitySource{src}, resolver, fixedNow)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	wantEnd := rideStart.Add(1 * time.Hour)
	if !merged.EndTime.Equal(wantEnd) {
		t.Errorf("EndTime = %v, want derived %v", merged.EndTime, wantEnd)
	}
}

func TestMerge_PrimaryStreamSourceIDIsWinner(t *testing.T) {
	// Two stream-bearing sources, plus one without a stream. The policy
	// for FieldGroupGPSTrack determines which is the primary.
	user := makeUserID("alice")
	actID := makeActivityID("ride8")

	garmin := makeGarminRide("g1", user, actID, 0)
	garmin.Parsed.HasStream = true
	strava := makeStravaRide("s1", user, actID, 0)
	strava.Parsed.HasStream = true
	manual := ActivitySource{
		ID: makeSourceID("m1"), ActivityID: actID, UserID: user,
		Provider: "manual_upload", ExternalID: "x",
		Parsed: ActivitySourcePayload{
			Type:            ActivityTypeRide,
			StartTime:       rideStart,
			EndTime:         rideStart.Add(1 * time.Hour),
			ElapsedDuration: 1 * time.Hour,
			MovingDuration:  1 * time.Hour,
			Timezone:        "Europe/Berlin",
			HasStream:       false, // summary-only manual entry
		},
		ImportedAt: fixedNow,
	}

	resolver := func(t ActivityType) MergePolicy { return testMergePolicyFor(t) }

	merged, err := Merge(actID, []ActivitySource{manual, strava, garmin}, resolver, fixedNow)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if merged.PrimaryStreamSourceID == nil {
		t.Fatal("PrimaryStreamSourceID is nil")
	}
	// Default ride policy ⇒ [garmin, strava, _any] for the GPS track group.
	if *merged.PrimaryStreamSourceID != garmin.ID {
		t.Errorf("PrimaryStreamSourceID = %s, want garmin %s",
			merged.PrimaryStreamSourceID, garmin.ID)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertSourceWon(t *testing.T, a Activity, g FieldGroup, want SourceID) {
	t.Helper()
	got, ok := a.MergeProvenance.Winner(g)
	if !ok {
		t.Errorf("group %s: no winner recorded", g)
		return
	}
	if got != want {
		t.Errorf("group %s: winner = %s, want %s", g, got, want)
	}
}

func TestMerge_PoolPriority_PicksOneImporterOfAProvider(t *testing.T) {
	// One Garmin account connected for two pools (docs/adr/0006): the API
	// importer and a FIT importer. "garmin.garmin-fit" prefers the FIT pool
	// for power even though the API source was imported later.
	user := makeUserID("alice")
	actID := makeActivityID("ride-pools")
	api := makeGarminRide("g-api", user, actID, -1*time.Hour)
	api.SourceWorkerName = "garmin-api"
	fit := makeGarminRide("g-fit", user, actID, -2*time.Hour)
	fit.SourceWorkerName = "garmin-fit"
	fit.ExternalID = "fit-1"
	fit.Parsed.Summary.AvgPowerW = Ptr(250)

	resolver := func(ActivityType) MergePolicy {
		return MergePolicy{
			DefaultPriority: []string{"garmin", AnyProvider},
			Overrides:       map[FieldGroup][]string{FieldGroupPower: {"garmin.garmin-fit", "garmin"}},
		}
	}
	merged, err := Merge(actID, []ActivitySource{api, fit}, resolver, fixedNow)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	assertSourceWon(t, merged, FieldGroupPower, fit.ID)
	// A bare provider entry still covers both pools: most recent import wins.
	assertSourceWon(t, merged, FieldGroupHeartRate, api.ID)

	// _any skips a source named by its pool.
	resolver = func(ActivityType) MergePolicy {
		return MergePolicy{DefaultPriority: []string{"garmin.garmin-api", AnyProvider}}
	}
	api.Parsed.Summary.AvgPowerW = nil
	api.Parsed.Summary.MaxPowerW = nil
	api.Parsed.Summary.NormalizedPowerW = nil
	merged, err = Merge(actID, []ActivitySource{api, fit}, resolver, fixedNow)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	assertSourceWon(t, merged, FieldGroupPower, fit.ID)
	assertSourceWon(t, merged, FieldGroupHeartRate, api.ID)
}
