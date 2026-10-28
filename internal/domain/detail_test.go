package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func i16(v int16) *int16 { return &v }

func TestComputeZones(t *testing.T) {
	start := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	// 20 min at 1 Hz: HR 150 (75 % of LTHR 200 → zone by band), power 200 (FTP 250).
	samples := make([]StreamSample, 1200)
	for i := range samples {
		samples[i] = StreamSample{Timestamp: start.Add(time.Duration(i) * time.Second), HeartRateBpm: i16(150), PowerW: i16(200)}
	}
	valAt := func(k AthleteMetricKey) (float64, bool) {
		switch k {
		case AthleteThresholdHR:
			return 200, true
		case AthleteFTPWatts:
			return 250, true
		}
		return 0, false
	}
	z := ComputeZones(samples, valAt)
	if z.HR == nil || z.HR.Basis != "lthr" || z.HR.Reference != 200 || z.HR.TotalSeconds != 1200 {
		t.Fatalf("hr %+v", z.HR)
	}
	if z.Power == nil || z.Power.Basis != "ftp" || z.Power.TotalSeconds != 1200 {
		t.Fatalf("power %+v", z.Power)
	}
	if z.Decoupling == nil || z.Decoupling.Basis != "power" || !z.Decoupling.Coupled {
		t.Fatalf("decoupling %+v", z.Decoupling)
	}
	// Max-HR fallback, no FTP, and a short effort: no decoupling.
	short := ComputeZones(samples[:100], func(k AthleteMetricKey) (float64, bool) {
		if k == AthleteMaxHR {
			return 190, true
		}
		return 0, false
	})
	if short.HR == nil || short.HR.Basis != "max" || short.Power != nil || short.Decoupling != nil {
		t.Fatalf("short %+v", short)
	}
	if r := ComputeZones(nil, valAt); r.HR != nil || r.Power != nil {
		t.Fatal("no samples → nothing")
	}
}

func TestReparseWorkerAndUpgrade(t *testing.T) {
	src := ActivitySource{Provider: "strava", SourceWorkerPackage: "strava", SourceWorkerVersion: "1.1", RawBlobID: "b"}
	workers := []LiveWorker{
		{Name: "a", Provider: "strava", Package: "strava", Version: "1.0", Compatible: true}, // older parser: cannot re-parse
		{Name: "b", Provider: "strava", Package: "strava", Version: "1.2", Compatible: true},
		{Name: "c", Provider: "strava", Package: "strava", Version: "1.3", Compatible: false}, // incompatible release
		{Name: "d", Provider: "garmin", Package: "garmin", Version: "1.5", Compatible: true},
	}
	w, ok := ReparseWorker(workers, src)
	if !ok || w.Name != "b" {
		t.Fatalf("got %+v %v", w, ok)
	}
	if SourceUpgrade(workers, src) != "new_parser" {
		t.Fatalf("upgrade %q", SourceUpgrade(workers, src))
	}
	if _, ok := ReparseWorker(workers, ActivitySource{Provider: "strava", SourceWorkerVersion: "1.1"}); ok {
		t.Fatal("legacy source without package must not re-parse")
	}
	if SourceUpgrade(workers, ActivitySource{Provider: "strava", SourceWorkerPackage: "strava", SourceWorkerVersion: "1.2"}) != "" {
		t.Fatal("current parser → no upgrade")
	}
}

func TestBuildManageInfo(t *testing.T) {
	s1, s2 := SourceID(uuid.New()), SourceID(uuid.New())
	act := Activity{ID: ActivityID(uuid.New()), PrimaryStreamSourceID: &s1, MergeProvenance: MergeProvenance{FieldGroupTitle: s1, FieldGroupDistance: s2}}
	sources := []ActivitySource{
		{ID: s1, Provider: "garmin", SourceWorkerPackage: "garmin", SourceWorkerVersion: "1.2", RawBlobID: "x"},
		{ID: s2, Provider: "strava", SourceWorkerVersion: "1", Status: SourceStatusDetached},
	}
	workers := []LiveWorker{{Provider: "garmin", Package: "garmin", Version: "1.3", Compatible: true}}
	info := BuildManageInfo(act, sources, workers, map[FieldGroup]bool{FieldGroupDistance: true})
	if len(info.Sources) != 2 || !info.Sources[0].IsPrimary || info.Sources[1].IsPrimary {
		t.Fatalf("sources %+v", info.Sources)
	}
	g := info.Sources[0]
	if g.ParserVersion != "1.2" || g.DatasetVersion != "1" || !g.ReparseEligible || g.ReparseWith != "1.3" || g.Upgrade != "new_parser" || len(g.WonFieldGroups) != 1 || g.WonFieldGroups[0] != "Title" {
		t.Fatalf("garmin insight %+v", g)
	}
	// A legacy "1" reads as parser 1.0 (docs/adr/0005).
	if st := info.Sources[1]; st.ParserVersion != "1.0" || st.DatasetVersion != "1" || st.ReparseEligible {
		t.Fatalf("strava insight %+v", st)
	}
	if len(info.Provenance) != 2 || info.Provenance[0].Field != FieldGroupDistance || info.Provenance[0].DecidedBy != "manual" || info.Provenance[1].DecidedBy != "rule" || info.Provenance[0].Provider != "strava" {
		t.Fatalf("provenance %+v", info.Provenance)
	}
}

func TestBuildSimilarGroup(t *testing.T) {
	d := 10000.0
	act := Activity{ID: ActivityID(uuid.New()), Title: "ref", Type: ActivityTypeRun, StartTime: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		MovingDuration: 50 * time.Minute, ElapsedDuration: time.Hour, Summary: ActivitySummary{DistanceM: &d}}
	matches := []SimilarActivity{
		{ActivityID: ActivityID(uuid.New()), Title: "faster", StartTime: act.StartTime.AddDate(0, 0, -7), DistanceM: &d, MovingS: 2700, SharedSegments: 2, TargetSegments: 4},
		{ActivityID: ActivityID(uuid.New()), Title: "slower", StartTime: act.StartTime.AddDate(0, 0, 7), DistanceM: &d, MovingS: 3300, SharedSegments: 4, TargetSegments: 4},
	}
	g := BuildSimilarGroup(act, matches)
	if g.TargetSegments != 4 || len(g.Rows) != 3 || g.Rows[0].Title != "faster" || !g.Rows[1].IsCurrent || g.Rows[2].Title != "slower" {
		t.Fatalf("rows %+v", g.Rows)
	}
	if g.BestMovingS != 2700 || g.CurrentMovingS != 3000 || g.CurrentRank != 2 || g.IsBest {
		t.Fatalf("rank %+v", g)
	}
	if g.Rows[1].PaceSPerKm == nil || *g.Rows[1].PaceSPerKm != 300 || g.Rows[1].OverlapPct != 1 || g.Rows[0].OverlapPct != 0.5 {
		t.Fatalf("pace/overlap %+v", g.Rows[1])
	}
	alone := BuildSimilarGroup(act, nil)
	if len(alone.Rows) != 1 || alone.CurrentRank != 1 || !alone.IsBest {
		t.Fatalf("alone %+v", alone)
	}
	if laps := PickLaps([]ActivitySource{{Parsed: ActivitySourcePayload{Laps: []ActivityLap{{}}}}, {Parsed: ActivitySourcePayload{Laps: []ActivityLap{{}, {}}}}}); len(laps) != 2 {
		t.Fatal("PickLaps must take the richest source")
	}
}
