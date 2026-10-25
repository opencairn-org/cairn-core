package match

import (
	"testing"
	"time"
)

// rec builds a Record from features with a given id.
func rec(id string, f SourceFeatures) Record { return Record{ID: id, SourceFeatures: f} }

// expectSame reports whether a fixture label means the pair should cluster.
func expectSame(label string) bool { return label == LabelSame || label == LabelIndoorNoGPS }

// TestScore_CorpusSeparation checks that the pairwise score cleanly separates
// "same" from "different" across the corpus: same pairs above the edge
// threshold (and not gated), different pairs below it (or gated).
func TestScore_CorpusSeparation(t *testing.T) {
	opt := DefaultOptions()
	for _, f := range Corpus() {
		s, gated := Score(f.A, f.B, opt.Weights)
		linked := !gated && s >= opt.EdgeThreshold
		if expectSame(f.Label) && !linked {
			t.Errorf("%s: expected SAME but score=%.3f gated=%v (< threshold %.2f)", f.Name, s, gated, opt.EdgeThreshold)
		}
		if !expectSame(f.Label) && linked {
			t.Errorf("%s: expected DIFFERENT but score=%.3f >= threshold %.2f", f.Name, s, opt.EdgeThreshold)
		}
	}
}

// TestClusterBucket_Corpus runs each fixture pair through the clusterer and
// checks the grouping matches the label, and reports overall precision/recall.
func TestClusterBucket_Corpus(t *testing.T) {
	opt := DefaultOptions()
	var tp, fp, fn, tn int
	for _, f := range Corpus() {
		res := ClusterBucket([]Record{rec("a", f.A), rec("b", f.B)}, Constraints{}, opt)
		clustered := len(res.Clusters) == 1
		switch {
		case expectSame(f.Label) && clustered:
			tp++
		case expectSame(f.Label) && !clustered:
			fn++
			t.Errorf("%s: expected to CLUSTER but got %d clusters", f.Name, len(res.Clusters))
		case !expectSame(f.Label) && clustered:
			fp++
			t.Errorf("%s: expected SEPARATE but clustered into one", f.Name)
		default:
			tn++
		}
	}
	t.Logf("corpus: tp=%d fp=%d fn=%d tn=%d", tp, fp, fn, tn)
}

// TestClusterBucket_OnePerProviderConflict: two same-provider near-identical
// records must NOT merge — that's an alarm, recorded as a Conflict.
func TestClusterBucket_OnePerProviderConflict(t *testing.T) {
	base := SourceFeatures{
		Provider: "garmin", SportClass: "Ride",
		StartUTC:  time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		DistanceM: 30000, MovingS: 3300, ElapsedS: 3600,
	}
	other := base
	other.StartUTC = base.StartUTC.Add(2 * time.Second)

	res := ClusterBucket([]Record{rec("a", base), rec("b", other)}, Constraints{}, DefaultOptions())
	if len(res.Clusters) != 2 {
		t.Errorf("same-provider pair must stay separate, got %d clusters", len(res.Clusters))
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(res.Conflicts))
	}
	if res.Conflicts[0].Provider != "garmin" {
		t.Errorf("conflict provider = %q, want garmin", res.Conflicts[0].Provider)
	}
}

// TestClusterBucket_MustLink forces two records that would NOT otherwise match
// (different sport, far apart) into one cluster.
func TestClusterBucket_MustLink(t *testing.T) {
	a := SourceFeatures{Provider: "garmin", SportClass: "Ride",
		StartUTC: time.Date(2026, 6, 2, 7, 0, 0, 0, time.UTC), DistanceM: 30000, MovingS: 3600, ElapsedS: 3700}
	b := SourceFeatures{Provider: "strava", SportClass: "Run",
		StartUTC: time.Date(2026, 6, 2, 18, 0, 0, 0, time.UTC), DistanceM: 8000, MovingS: 2400, ElapsedS: 2450}

	// Without constraint: separate.
	if res := ClusterBucket([]Record{rec("a", a), rec("b", b)}, Constraints{}, DefaultOptions()); len(res.Clusters) != 2 {
		t.Fatalf("precondition: expected 2 clusters, got %d", len(res.Clusters))
	}
	// With must-link: one cluster.
	res := ClusterBucket([]Record{rec("a", a), rec("b", b)}, Constraints{MustLink: [][2]string{{"a", "b"}}}, DefaultOptions())
	if len(res.Clusters) != 1 {
		t.Errorf("must-link should force one cluster, got %d", len(res.Clusters))
	}
}

// TestClusterBucket_CannotLink splits a pair that WOULD otherwise cluster.
func TestClusterBucket_CannotLink(t *testing.T) {
	f := Corpus()[0] // garmin_strava_autopush_ride (same)
	if f.Label != LabelSame {
		t.Skip("corpus[0] changed")
	}
	// precondition: clusters into one.
	if res := ClusterBucket([]Record{rec("a", f.A), rec("b", f.B)}, Constraints{}, DefaultOptions()); len(res.Clusters) != 1 {
		t.Fatalf("precondition: expected 1 cluster, got %d", len(res.Clusters))
	}
	res := ClusterBucket([]Record{rec("a", f.A), rec("b", f.B)},
		Constraints{CannotLink: [][2]string{{"a", "b"}}}, DefaultOptions())
	if len(res.Clusters) != 2 {
		t.Errorf("cannot-link should force two clusters, got %d", len(res.Clusters))
	}
}

// TestClusterBucket_Deterministic: same input + reordered input → identical
// clusters (sorted output, ID tie-breaks).
func TestClusterBucket_Deterministic(t *testing.T) {
	f := Corpus()[0]
	in1 := []Record{rec("a", f.A), rec("b", f.B)}
	in2 := []Record{rec("b", f.B), rec("a", f.A)}
	r1 := ClusterBucket(in1, Constraints{}, DefaultOptions())
	r2 := ClusterBucket(in2, Constraints{}, DefaultOptions())
	if len(r1.Clusters) != len(r2.Clusters) {
		t.Fatalf("cluster counts differ: %d vs %d", len(r1.Clusters), len(r2.Clusters))
	}
	for i := range r1.Clusters {
		if len(r1.Clusters[i].RecordIDs) != len(r2.Clusters[i].RecordIDs) {
			t.Fatalf("cluster %d size differs", i)
		}
		for j := range r1.Clusters[i].RecordIDs {
			if r1.Clusters[i].RecordIDs[j] != r2.Clusters[i].RecordIDs[j] {
				t.Errorf("cluster %d member %d differs: %s vs %s", i, j,
					r1.Clusters[i].RecordIDs[j], r2.Clusters[i].RecordIDs[j])
			}
		}
	}
}

// TestClusterBucket_ThreeProviderSameActivity: a Garmin+Strava+manual trio of
// the same ride clusters into one (each a different provider, so no conflict).
func TestClusterBucket_ThreeProviderSameActivity(t *testing.T) {
	start := time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC)
	mk := func(p string, off time.Duration, dist float64) SourceFeatures {
		return SourceFeatures{Provider: p, SportClass: "Ride", StartUTC: start.Add(off),
			DistanceM: dist, MovingS: 3300, ElapsedS: 3600}
	}
	recs := []Record{
		rec("g", mk("garmin", 0, 30050)),
		rec("s", mk("strava", 2*time.Second, 30000)),
		rec("m", mk("manual_upload", 1*time.Second, 30020)),
	}
	res := ClusterBucket(recs, Constraints{}, DefaultOptions())
	if len(res.Clusters) != 1 || len(res.Clusters[0].RecordIDs) != 3 {
		t.Fatalf("expected one 3-member cluster, got %+v", res.Clusters)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("no conflicts expected (all distinct providers), got %d", len(res.Conflicts))
	}
}

func TestClusterBucket_OneSourcePerPoolNotPerProvider(t *testing.T) {
	lat, lng := 52.5, 13.4
	f := SourceFeatures{Provider: "garmin", SportClass: "Ride", StartUTC: time.Date(2026, 7, 4, 13, 40, 0, 0, time.UTC),
		DistanceM: 42000, MovingS: 5400, ElapsedS: 5600, StartLat: &lat, StartLng: &lng}
	// Two pools of one provider (API + FIT importer) report the same ride.
	res := ClusterBucket([]Record{{ID: "api", Pool: "garmin-api", SourceFeatures: f}, {ID: "fit", Pool: "garmin-fit", SourceFeatures: f}}, Constraints{}, DefaultOptions())
	if len(res.Clusters) != 1 || len(res.Clusters[0].RecordIDs) != 2 || res.Clusters[0].Confidence != BandHigh || len(res.Conflicts) != 0 {
		t.Fatalf("two pools: %+v", res)
	}
	// The same pool twice stays a conflict.
	res = ClusterBucket([]Record{{ID: "a", Pool: "garmin-api", SourceFeatures: f}, {ID: "b", Pool: "garmin-api", SourceFeatures: f}}, Constraints{}, DefaultOptions())
	if len(res.Clusters) != 2 || len(res.Conflicts) != 1 {
		t.Fatalf("same pool: %+v", res)
	}
	// Without pools the provider is the identity (pre-0.5 behaviour).
	res = ClusterBucket([]Record{rec("a", f), rec("b", f)}, Constraints{}, DefaultOptions())
	if len(res.Conflicts) != 1 {
		t.Fatalf("no pools: %+v", res)
	}
}
