// Package besteffort holds the sliding-window best-effort computation
// use case. The output rows feed the per-activity power/pace curves on
// the activity-detail page and the per-user PR queries.
package besteffort

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ComputeBestEffortsForSource takes one ActivitySource, reads its raw
// stream, computes peak averages over the standard windows for each
// supported metric, and persists the result.
//
// Idempotent: a re-run (after a reimport, or after the user changed the
// activity's discipline) deletes the source's prior rows and writes the
// fresh set in a single transaction.
//
// Assumptions (v1):
//
//   - Streams are ~1Hz. Windows are interpreted as "this many consecutive
//     samples". Sparse or variable-rate streams will produce approximate
//     results; that's acceptable for v1 since most workers up-sample to 1Hz.
//   - Only duration windows are computed. Distance-based pace windows
//     (1km, 5k, marathon) need cumulative-distance traversal, which is
//     a v2 feature.
//   - Windows requiring more samples than the stream contains are skipped
//     silently — short activities legitimately have no "20 minute power".
type ComputeBestEffortsForSource struct {
	activities  port.ActivityRepo
	streams     port.StreamRepo
	bestEfforts port.BestEffortRepo
	tx          port.TxManager

	now   func() time.Time
	newID func() uuid.UUID
}

func NewComputeBestEffortsForSource(
	activities port.ActivityRepo,
	streams port.StreamRepo,
	bestEfforts port.BestEffortRepo,
	tx port.TxManager,
	now func() time.Time,
	newID func() uuid.UUID,
) *ComputeBestEffortsForSource {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if newID == nil {
		newID = func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		}
	}
	return &ComputeBestEffortsForSource{
		activities:  activities,
		streams:     streams,
		bestEfforts: bestEfforts,
		tx:          tx,
		now:         now,
		newID:       newID,
	}
}

// Input identifies the source to compute for.
type Input struct {
	ActivitySourceID domain.SourceID
}

// Result reports what the compute produced. It is non-fatal to return
// EffortsCount == 0 — short or stream-less activities legitimately have
// no efforts.
type Result struct {
	EffortsCount int
	NoStream     bool // source has no stored stream samples
	// NewRecords lists the all-time personal records this source now holds
	// that it did not hold before the compute (live "new PR" events).
	NewRecords []domain.PersonalRecord
}

// ---------------------------------------------------------------------------
// Standard windows
// ---------------------------------------------------------------------------

// windowSpec is one (metric, window-kind, window-value) combination the
// compute scans. The value is interpreted by Kind: seconds for duration
// windows, metres for distance windows.
type windowSpec struct {
	Metric domain.BestEffortMetric
	Kind   domain.BestEffortWindowKind
	Value  int
}

// StandardWindows is the v1 set. Duration windows are sample-count based
// (assumes ~1Hz); distance windows are cumulative-distance based via a
// two-pointer scan.
var StandardWindows = []windowSpec{
	// --- Duration windows ---
	// Cycling power — the canonical 5s/30s/1m/5m/20m/60m curve.
	{domain.BestEffortMetricPower, domain.BestEffortWindowDuration, 5},
	{domain.BestEffortMetricPower, domain.BestEffortWindowDuration, 30},
	{domain.BestEffortMetricPower, domain.BestEffortWindowDuration, 60},
	{domain.BestEffortMetricPower, domain.BestEffortWindowDuration, 300},
	{domain.BestEffortMetricPower, domain.BestEffortWindowDuration, 1200},
	{domain.BestEffortMetricPower, domain.BestEffortWindowDuration, 3600},
	// Heart rate — fewer windows since HR averages don't change as quickly.
	{domain.BestEffortMetricHeartRate, domain.BestEffortWindowDuration, 60},
	{domain.BestEffortMetricHeartRate, domain.BestEffortWindowDuration, 300},
	{domain.BestEffortMetricHeartRate, domain.BestEffortWindowDuration, 1200},
	{domain.BestEffortMetricHeartRate, domain.BestEffortWindowDuration, 3600},
	// Speed — relevant for cycling activities without a power meter.
	{domain.BestEffortMetricSpeed, domain.BestEffortWindowDuration, 60},
	{domain.BestEffortMetricSpeed, domain.BestEffortWindowDuration, 300},
	{domain.BestEffortMetricSpeed, domain.BestEffortWindowDuration, 1200},

	// VAM — vertical ascent velocity (m/h). Best sustained climbing rate over
	// classic climb durations. Computed from the altitude channel as a rate
	// (Δaltitude / window), not an instantaneous average.
	{domain.BestEffortMetricVAM, domain.BestEffortWindowDuration, 300},
	{domain.BestEffortMetricVAM, domain.BestEffortWindowDuration, 1200},
	{domain.BestEffortMetricVAM, domain.BestEffortWindowDuration, 3600},

	// --- Distance windows (pace) ---
	// Running's canonical PR distances. Computed for any activity whose
	// stream carries cumulative distance — cyclists also get a "fastest
	// 5km" record this way, which is harmless if useless.
	{domain.BestEffortMetricPace, domain.BestEffortWindowDistance, 400},
	{domain.BestEffortMetricPace, domain.BestEffortWindowDistance, 1000},
	{domain.BestEffortMetricPace, domain.BestEffortWindowDistance, 1609}, // 1 mile
	{domain.BestEffortMetricPace, domain.BestEffortWindowDistance, 5000},
	{domain.BestEffortMetricPace, domain.BestEffortWindowDistance, 10000},
	{domain.BestEffortMetricPace, domain.BestEffortWindowDistance, 21097}, // half marathon
	{domain.BestEffortMetricPace, domain.BestEffortWindowDistance, 42195}, // marathon
}

// ---------------------------------------------------------------------------
// Execute
// ---------------------------------------------------------------------------

func (uc *ComputeBestEffortsForSource) Execute(ctx context.Context, in Input) (Result, error) {
	// Read context (source → activity → user).
	src, err := uc.activities.GetSource(ctx, in.ActivitySourceID)
	if err != nil {
		return Result{}, fmt.Errorf("load source %s: %w", in.ActivitySourceID, err)
	}
	act, err := uc.activities.GetActivity(ctx, src.ActivityID)
	if err != nil {
		return Result{}, fmt.Errorf("load activity %s: %w", src.ActivityID, err)
	}

	stream, err := uc.streams.QueryStream(ctx, domain.StreamQuery{
		ActivitySourceID: in.ActivitySourceID,
		Resolution:       domain.StreamResolutionRaw,
	})
	if err != nil {
		return Result{}, fmt.Errorf("read stream for %s: %w", in.ActivitySourceID, err)
	}
	if len(stream.Samples) == 0 {
		// Persist the "no efforts" state by wiping any prior efforts. This
		// keeps the table consistent if a previous import had a stream and
		// a reimport doesn't.
		if err := uc.tx.InTx(ctx, func(ctx context.Context) error {
			return uc.bestEfforts.DeleteForSource(ctx, in.ActivitySourceID)
		}); err != nil {
			return Result{}, fmt.Errorf("clear efforts: %w", err)
		}
		return Result{NoStream: true}, nil
	}

	// Compute.
	efforts := uc.computeAll(act, src, stream.Samples)
	before, _ := uc.bestEfforts.ListPersonalRecords(ctx, act.UserID) // best-effort: only feeds live events

	// Persist atomically.
	err = uc.tx.InTx(ctx, func(ctx context.Context) error {
		if err := uc.bestEfforts.DeleteForSource(ctx, in.ActivitySourceID); err != nil {
			return err
		}
		return uc.bestEfforts.SaveEfforts(ctx, efforts)
	})
	if err != nil {
		return Result{}, fmt.Errorf("persist efforts: %w", err)
	}
	after, _ := uc.bestEfforts.ListPersonalRecords(ctx, act.UserID)
	return Result{EffortsCount: len(efforts), NewRecords: newRecords(before, after, act.ID)}, nil
}

// newRecords returns the records of after held by activityID that before
// did not attribute to it.
func newRecords(before, after []domain.PersonalRecord, activityID domain.ActivityID) []domain.PersonalRecord {
	type key struct {
		t domain.ActivityType
		m domain.BestEffortMetric
		k domain.BestEffortWindowKind
		v int
	}
	prev := map[key]domain.ActivityID{}
	for _, r := range before {
		prev[key{r.ActivityType, r.Metric, r.WindowKind, r.WindowValue}] = r.ActivityID
	}
	var out []domain.PersonalRecord
	for _, r := range after {
		if r.ActivityID != activityID {
			continue
		}
		if prev[key{r.ActivityType, r.Metric, r.WindowKind, r.WindowValue}] != activityID {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Compute pipeline
// ---------------------------------------------------------------------------

// computeAll runs each StandardWindow against the source's samples and
// returns the populated BestEffort rows. Dispatches by Kind: duration
// windows use the sample-count sliding-window scan; distance windows use
// a two-pointer scan over the cumulative-distance channel.
func (uc *ComputeBestEffortsForSource) computeAll(
	act domain.Activity,
	src domain.ActivitySource,
	samples []domain.StreamSample,
) []domain.BestEffort {
	now := uc.now()
	var out []domain.BestEffort
	for _, w := range StandardWindows {
		switch w.Kind {
		case domain.BestEffortWindowDuration:
			if be, ok := uc.computeDurationEffort(act, src, samples, w, now); ok {
				out = append(out, be)
			}
		case domain.BestEffortWindowDistance:
			if be, ok := uc.computeDistanceEffort(act, src, samples, w, now); ok {
				out = append(out, be)
			}
		}
	}
	return out
}

// computeDurationEffort handles the sample-count sliding-window case.
func (uc *ComputeBestEffortsForSource) computeDurationEffort(
	act domain.Activity,
	src domain.ActivitySource,
	samples []domain.StreamSample,
	w windowSpec,
	now time.Time,
) (domain.BestEffort, bool) {
	// VAM is a rate (Δaltitude / window-hours), not an instantaneous channel —
	// it gets its own elevation-delta sliding window.
	if w.Metric == domain.BestEffortMetricVAM {
		return uc.computeVAMEffort(act, src, samples, w, now)
	}
	values := extractChannel(samples, w.Metric)
	if values == nil {
		return domain.BestEffort{}, false
	}
	avg, startIdx, ok := slidingWindowMax(values, w.Value)
	if !ok {
		return domain.BestEffort{}, false
	}
	endIdx := startIdx + w.Value - 1
	if endIdx >= len(samples) {
		endIdx = len(samples) - 1
	}
	startTS := samples[startIdx].Timestamp
	endTS := samples[endIdx].Timestamp

	return domain.BestEffort{
		ID:               domain.BestEffortID(uc.newID()),
		ActivityID:       act.ID,
		ActivitySourceID: src.ID,
		UserID:           act.UserID,
		ActivityType:     act.Type,
		Discipline:       act.Discipline,
		Metric:           w.Metric,
		WindowKind:       domain.BestEffortWindowDuration,
		WindowValue:      w.Value,
		AchievedValue:    avg,
		StartOffset:      int(startTS.Sub(samples[0].Timestamp).Seconds()),
		DurationS:        endTS.Sub(startTS).Seconds(),
		Timestamp:        startTS,
		CreatedAt:        now,
	}, true
}

// computeVAMEffort finds the best sustained vertical-ascent rate (m/h) over the
// window: max over all start points of (altitudeGain over the window) / hours.
// Requires the altitude channel; skips when there's no net climb in any window.
func (uc *ComputeBestEffortsForSource) computeVAMEffort(
	act domain.Activity,
	src domain.ActivitySource,
	samples []domain.StreamSample,
	w windowSpec,
	now time.Time,
) (domain.BestEffort, bool) {
	alt := extractAltitude(samples)
	if alt == nil || len(alt) < w.Value {
		return domain.BestEffort{}, false
	}
	hours := float64(w.Value) / 3600.0
	best := math.Inf(-1)
	bestIdx := 0
	for i := 0; i+w.Value-1 < len(alt); i++ {
		// Skip windows where either endpoint is missing altitude.
		a0, a1 := alt[i], alt[i+w.Value-1]
		if math.IsNaN(a0) || math.IsNaN(a1) {
			continue
		}
		if vam := (a1 - a0) / hours; vam > best {
			best = vam
			bestIdx = i
		}
	}
	if math.IsInf(best, -1) || best <= 0 { // no climbing window
		return domain.BestEffort{}, false
	}
	endIdx := bestIdx + w.Value - 1
	if endIdx >= len(samples) {
		endIdx = len(samples) - 1
	}
	startTS := samples[bestIdx].Timestamp
	endTS := samples[endIdx].Timestamp
	return domain.BestEffort{
		ID:               domain.BestEffortID(uc.newID()),
		ActivityID:       act.ID,
		ActivitySourceID: src.ID,
		UserID:           act.UserID,
		ActivityType:     act.Type,
		Discipline:       act.Discipline,
		Metric:           domain.BestEffortMetricVAM,
		WindowKind:       domain.BestEffortWindowDuration,
		WindowValue:      w.Value,
		AchievedValue:    best,
		StartOffset:      int(startTS.Sub(samples[0].Timestamp).Seconds()),
		DurationS:        endTS.Sub(startTS).Seconds(),
		Timestamp:        startTS,
		CreatedAt:        now,
	}, true
}

// extractAltitude pulls the altitude channel (NaN where missing). Returns nil
// when fewer than 5 samples carry altitude.
func extractAltitude(samples []domain.StreamSample) []float64 {
	out := make([]float64, len(samples))
	valid := 0
	for i, s := range samples {
		if s.AltitudeM != nil {
			out[i] = *s.AltitudeM
			valid++
		} else {
			out[i] = math.NaN()
		}
	}
	if valid < 5 {
		return nil
	}
	return out
}

// computeDistanceEffort handles the cumulative-distance two-pointer case.
// Only pace is supported in v1 (the only distance metric in StandardWindows);
// other metrics would need their own per-distance scan.
func (uc *ComputeBestEffortsForSource) computeDistanceEffort(
	act domain.Activity,
	src domain.ActivitySource,
	samples []domain.StreamSample,
	w windowSpec,
	now time.Time,
) (domain.BestEffort, bool) {
	if w.Metric != domain.BestEffortMetricPace {
		return domain.BestEffort{}, false
	}
	pace, startIdx, endIdx, dist, ok := slidingWindowMinPace(samples, float64(w.Value))
	if !ok {
		return domain.BestEffort{}, false
	}
	startTS := samples[startIdx].Timestamp
	endTS := samples[endIdx].Timestamp
	distance := dist // captured before pointer-take
	return domain.BestEffort{
		ID:               domain.BestEffortID(uc.newID()),
		ActivityID:       act.ID,
		ActivitySourceID: src.ID,
		UserID:           act.UserID,
		ActivityType:     act.Type,
		Discipline:       act.Discipline,
		Metric:           w.Metric,
		WindowKind:       domain.BestEffortWindowDistance,
		WindowValue:      w.Value,
		AchievedValue:    pace,
		StartOffset:      int(startTS.Sub(samples[0].Timestamp).Seconds()),
		DurationS:        endTS.Sub(startTS).Seconds(),
		DistanceM:        &distance,
		Timestamp:        startTS,
		CreatedAt:        now,
	}, true
}

// extractChannel pulls the per-metric scalar series out of the stream
// samples. Returns nil when the stream has fewer than 5 valid points for
// the requested metric — too sparse to compute meaningfully.
//
// NaN markers represent "this sample didn't carry the channel"; the
// sliding-window routine handles them.
func extractChannel(samples []domain.StreamSample, metric domain.BestEffortMetric) []float64 {
	out := make([]float64, len(samples))
	validCount := 0
	for i, s := range samples {
		v, ok := readMetric(s, metric)
		if !ok {
			out[i] = math.NaN()
			continue
		}
		out[i] = v
		validCount++
	}
	if validCount < 5 {
		return nil
	}
	return out
}

func readMetric(s domain.StreamSample, metric domain.BestEffortMetric) (float64, bool) {
	switch metric {
	case domain.BestEffortMetricPower:
		if s.PowerW == nil {
			return 0, false
		}
		return float64(*s.PowerW), true
	case domain.BestEffortMetricHeartRate:
		if s.HeartRateBpm == nil {
			return 0, false
		}
		return float64(*s.HeartRateBpm), true
	case domain.BestEffortMetricSpeed:
		if s.SpeedMps == nil {
			return 0, false
		}
		return *s.SpeedMps, true
	default:
		// pace uses the distance two-pointer scan; vam its own altitude-rate
		// scan (computeVAMEffort). Neither is an instantaneous channel here.
		return 0, false
	}
}

// slidingWindowMax scans values[] with a sliding window of `windowSec`
// consecutive samples (assumes 1Hz sampling) and returns
// (best-average, start-index, ok).
//
// Samples carrying math.NaN are excluded from the average. A window is
// only considered "valid" if at least 80% of its samples are non-NaN —
// this prevents a 5-second window of mostly-missing data from scoring as
// "the user's best 5-second power" purely because the average over a
// tiny non-NaN tail is high.
func slidingWindowMax(values []float64, windowSec int) (avg float64, startIdx int, ok bool) {
	if len(values) < windowSec {
		return 0, 0, false
	}
	minValid := windowSec * 80 / 100
	if minValid < 1 {
		minValid = 1
	}

	// Prefix sums + valid-count arrays for O(N) scanning.
	sums := make([]float64, len(values)+1)
	valid := make([]int, len(values)+1)
	for i, v := range values {
		if math.IsNaN(v) {
			sums[i+1] = sums[i]
			valid[i+1] = valid[i]
		} else {
			sums[i+1] = sums[i] + v
			valid[i+1] = valid[i] + 1
		}
	}

	var (
		best  float64
		bestS int
		found bool
	)
	for start := 0; start+windowSec <= len(values); start++ {
		wValid := valid[start+windowSec] - valid[start]
		if wValid < minValid {
			continue
		}
		wSum := sums[start+windowSec] - sums[start]
		windowAvg := wSum / float64(wValid)
		if !found || windowAvg > best {
			best, bestS, found = windowAvg, start, true
		}
	}
	return best, bestS, found
}

// paceDistancePoint is one sample that carries cumulative distance.
type paceDistancePoint struct {
	idx  int
	dist float64
	ts   time.Time
}

// paceDistancePoints is the compact view of samples that carry distance.
func paceDistancePoints(samples []domain.StreamSample) []paceDistancePoint {
	pts := make([]paceDistancePoint, 0, len(samples))
	for i, s := range samples {
		if s.DistanceM == nil {
			continue
		}
		pts = append(pts, paceDistancePoint{idx: i, dist: *s.DistanceM, ts: s.Timestamp})
	}
	return pts
}

// paceAdvanceEnd advances end until the window from start covers windowDistanceM.
// end is monotone across outer steps — never reset.
func paceAdvanceEnd(pts []paceDistancePoint, start, end int, windowDistanceM float64) int {
	if end < start {
		end = start
	}
	for end < len(pts) && pts[end].dist-pts[start].dist < windowDistanceM {
		end++
	}
	return end
}

// slidingWindowMinPace finds the fastest cumulative-distance window of
// at least windowDistanceM metres. Returns the pace in seconds per km
// over the window, the original-array start/end indices, the actual
// distance covered (always >= windowDistanceM), and ok.
//
// Two-pointer scan in O(N):
//
//   - Build a compact view of (idx, dist, ts) over samples that carry
//     cumulative DistanceM. Samples without it are skipped.
//   - For each `start`, advance `end` until the inter-sample distance
//     covers the window. Compute pace; track the minimum.
//   - When `end` runs off the array, no further windows fit — break.
//
// "Fastest" means LOWEST seconds-per-km. The use case persists the
// resulting BestEffort with Metric = pace; SmallerIsBetter == true on
// the metric tells leaderboard sorters to ORDER BY achieved_value ASC.
func slidingWindowMinPace(samples []domain.StreamSample, windowDistanceM float64) (paceSecPerKm float64, startIdx, endIdx int, distM float64, ok bool) {
	if len(samples) < 2 || windowDistanceM <= 0 {
		return 0, 0, 0, 0, false
	}

	pts := paceDistancePoints(samples)
	if len(pts) < 2 {
		return 0, 0, 0, 0, false
	}
	if pts[len(pts)-1].dist-pts[0].dist < windowDistanceM {
		return 0, 0, 0, 0, false
	}

	var (
		bestPace  float64
		bestStart = pts[0].idx
		bestEnd   = pts[0].idx
		bestDist  float64
		found     bool
	)
	end := 0
	for start := 0; start < len(pts); start++ {
		end = paceAdvanceEnd(pts, start, end, windowDistanceM)
		if end >= len(pts) {
			break // no further windows can cover the distance
		}
		elapsed := pts[end].ts.Sub(pts[start].ts).Seconds()
		dist := pts[end].dist - pts[start].dist
		if elapsed <= 0 || dist <= 0 {
			continue
		}
		pace := elapsed / (dist / 1000.0)
		if !found || pace < bestPace {
			bestPace = pace
			bestStart = pts[start].idx
			bestEnd = pts[end].idx
			bestDist = dist
			found = true
		}
	}
	return bestPace, bestStart, bestEnd, bestDist, found
}
