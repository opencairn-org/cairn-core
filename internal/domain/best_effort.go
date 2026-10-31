package domain

import "time"

// ---------------------------------------------------------------------------
// BestEffort
//
// Sliding-window peak computed from one ActivitySource's stream. One row
// per (source, metric, window_kind, window_value). Reimports replace
// per-source rows; per-user PRs are queried by ORDER BY achieved_value
// against the indexed columns (best_efforts_user_lookup_idx).
//
// Computed by usecase/besteffort/compute.go after each source ingest.
// ---------------------------------------------------------------------------

type BestEffort struct {
	ID               BestEffortID
	ActivityID       ActivityID
	ActivitySourceID SourceID
	UserID           UserID

	ActivityType ActivityType
	Discipline   Discipline // "" if the activity has no discipline set

	Metric      BestEffortMetric
	WindowKind  BestEffortWindowKind
	WindowValue int // meters for WindowDistance, seconds for WindowDuration

	// AchievedValue uses metric-specific units (see BestEffortMetric).
	AchievedValue float64

	// StartOffset is the offset in seconds from the source's stream start
	// where the effort begins. Lets the UI scrub the chart to the highlight.
	StartOffset int
	DurationS   float64
	DistanceM   *float64 // populated for WindowDistance efforts

	// Timestamp is the absolute UTC time the effort started — used for
	// "PR over time" charts.
	Timestamp time.Time

	CreatedAt time.Time
}

// BestEffortMetric — the channel/computation a best-effort represents.
//
//	pace        — running pace, seconds per kilometre (smaller is better)
//	speed       — speed in metres per second (larger is better)
//	power       — cycling power, watts (larger is better)
//	heart_rate  — average BPM over the window (larger ≈ harder effort)
//	vam         — vertical ascent metres per hour (larger is better)
type BestEffortMetric string

const (
	BestEffortMetricPace      BestEffortMetric = "pace"
	BestEffortMetricSpeed     BestEffortMetric = "speed"
	BestEffortMetricPower     BestEffortMetric = "power"
	BestEffortMetricHeartRate BestEffortMetric = "heart_rate"
	BestEffortMetricVAM       BestEffortMetric = "vam"
)

// BestEffortMetrics lists every metric.
var BestEffortMetrics = []BestEffortMetric{
	BestEffortMetricPace, BestEffortMetricSpeed, BestEffortMetricPower, BestEffortMetricHeartRate, BestEffortMetricVAM,
}

// SmallerIsBetter reports whether a smaller AchievedValue is the better
// effort for this metric. Used by user-record queries to ORDER BY ASC
// vs DESC.
func (m BestEffortMetric) SmallerIsBetter() bool {
	return m == BestEffortMetricPace
}

// SmallerIsBetterMetrics lists the metrics where the smallest value wins.
func SmallerIsBetterMetrics() []BestEffortMetric {
	var out []BestEffortMetric
	for _, m := range BestEffortMetrics {
		if m.SmallerIsBetter() {
			out = append(out, m)
		}
	}
	return out
}

// PersonalRecord is the user's all-time best for one (activity type, metric,
// window) across EVERY activity and EVERY provider — the unified cross-provider
// personal best the brief (§11) calls out as a headline feature. Derived from
// best_efforts (which already pick the best value across an activity's sources),
// so it reflects the merged canonical view and is always consistent with the
// current data (computed on read; no materialization to invalidate).
type PersonalRecord struct {
	ActivityType  ActivityType
	Metric        BestEffortMetric
	WindowKind    BestEffortWindowKind
	WindowValue   int
	AchievedValue float64
	ActivityID    ActivityID
	Timestamp     time.Time
}

// BestEffortWindowKind — distance windows (e.g. 5k) vs duration windows
// (e.g. 20-minute power).
type BestEffortWindowKind string

const (
	BestEffortWindowDistance BestEffortWindowKind = "distance"
	BestEffortWindowDuration BestEffortWindowKind = "duration"
)
