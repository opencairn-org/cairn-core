package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// MetricBucket is the aggregation interval of a series query.
type MetricBucket string

const (
	MetricBucketNone  MetricBucket = ""
	MetricBucketHour  MetricBucket = "hour"
	MetricBucketDay   MetricBucket = "day"
	MetricBucketWeek  MetricBucket = "week"
	MetricBucketMonth MetricBucket = "month"
)

// Duration is the bucket width; zero for raw points.
func (b MetricBucket) Duration() time.Duration {
	switch b {
	case MetricBucketHour:
		return time.Hour
	case MetricBucketDay:
		return 24 * time.Hour
	case MetricBucketWeek:
		return 7 * 24 * time.Hour
	case MetricBucketMonth:
		return 30 * 24 * time.Hour
	}
	return 0
}

// MetricAggregation folds the samples of one bucket into one value.
type MetricAggregation string

const (
	MetricAggAvg   MetricAggregation = "avg"
	MetricAggMin   MetricAggregation = "min"
	MetricAggMax   MetricAggregation = "max"
	MetricAggSum   MetricAggregation = "sum"
	MetricAggLast  MetricAggregation = "last"
	MetricAggFirst MetricAggregation = "first"
	MetricAggCount MetricAggregation = "count"
)

// MetricSeriesQuery describes one charting read.
type MetricSeriesQuery struct {
	Key         string
	Start, End  time.Time
	Bucket      MetricBucket
	Aggregation MetricAggregation // ignored without a bucket
	Providers   []string
	MaxPoints   int // 0 = no cap
}

// MetricSeriesPoint is one (possibly aggregated) sample.
type MetricSeriesPoint struct {
	Ts      time.Time
	Value   float64
	Samples int
}

// MetricListFilter narrows a paged metric listing.
type MetricListFilter struct {
	Keys              []string
	Start, End        time.Time
	Providers         []string
	ExternalAccountID *ExternalAccountID
	ActivityID        *ActivityID
	Ascending         bool
	Limit, Offset     int
}

// MetricProviderManual tags user-entered metrics.
const MetricProviderManual = "manual"

const (
	maxMetricKeyLen   = 64
	maxMetricNotesLen = 2000
	maxMetricTags     = 20
)

var (
	ErrMetricKeyInvalid    = errors.New("metric key must be 1-64 chars of a-z, 0-9, '_' or '.'")
	ErrMetricValueMissing  = errors.New("exactly one of value_numeric or value_struct is required")
	ErrMetricShapeMismatch = errors.New("metric key expects a different value shape")
	ErrMetricTimestamp     = errors.New("metric timestamp is required")
	ErrMetricPeriod        = errors.New("period_seconds must not be negative")
	ErrMetricNotes         = errors.New("notes too long")
	ErrMetricTags          = errors.New("too many tags")
	ErrMetricSeriesKey     = errors.New("series key is required")
	ErrMetricSeriesRange   = errors.New("series range end must be after start")
	ErrMetricBucket        = errors.New("unknown metric bucket")
	ErrMetricAggregation   = errors.New("unknown metric aggregation")
)

var metricKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.]*$`)

// structMetricKeys lists the reserved keys whose value is structured.
var structMetricKeys = map[string]bool{"sleep_stages": true}

// reservedNumericMetricKeys lists the documented scalar keys (metrics.proto).
var reservedNumericMetricKeys = map[string]bool{
	"weight_kg": true, "body_fat_pct": true, "lean_mass_kg": true, "bone_mass_kg": true, "hydration_pct": true, "muscle_mass_kg": true,
	"resting_hr_bpm": true, "max_hr_bpm": true, "hrv_rmssd_ms": true, "hrv_sdnn_ms": true,
	"blood_pressure_systolic_mmhg": true, "blood_pressure_diastolic_mmhg": true, "spo2_pct": true,
	"steps": true, "floors_climbed": true, "active_minutes": true, "calories_burned_kcal": true,
	"sleep_total_min": true, "sleep_deep_min": true, "sleep_rem_min": true, "sleep_light_min": true, "sleep_awake_min": true, "sleep_score": true,
	"vo2max_ml_kg_min": true, "ftp_cycling_w": true, "lthr_running_bpm": true, "lthr_cycling_bpm": true,
	"threshold_pace_running_s_per_km": true, "css_swimming_s_per_100m": true, "critical_power_w": true, "w_prime_kj": true,
	"tss": true, "ctl": true, "atl": true, "tsb": true,
	"mood": true, "energy": true, "soreness": true, "stress": true, "motivation": true,
}

// ValidateMetricKey checks the key syntax.
func ValidateMetricKey(key string) error {
	if len(key) == 0 || len(key) > maxMetricKeyLen || !metricKeyRe.MatchString(key) {
		return ErrMetricKeyInvalid
	}
	return nil
}

// Validate checks a metric before it is stored: key syntax, one value of the
// shape the reserved keys dictate, a timestamp and sane sizes.
func (m Metric) Validate() error {
	if err := ValidateMetricKey(m.Key); err != nil {
		return err
	}
	hasNum, hasStruct := m.ValueNumeric != nil, len(m.ValueStruct) > 0
	if hasNum == hasStruct {
		return ErrMetricValueMissing
	}
	if (structMetricKeys[m.Key] && hasNum) || (reservedNumericMetricKeys[m.Key] && hasStruct) {
		return ErrMetricShapeMismatch
	}
	if m.Timestamp.IsZero() {
		return ErrMetricTimestamp
	}
	if m.PeriodSeconds < 0 {
		return ErrMetricPeriod
	}
	if len(m.Notes) > maxMetricNotesLen {
		return ErrMetricNotes
	}
	if len(m.Tags) > maxMetricTags {
		return ErrMetricTags
	}
	return nil
}

// Validate checks a series query and fills the defaults (avg aggregation).
func (q *MetricSeriesQuery) Validate() error {
	q.Key = strings.TrimSpace(q.Key)
	if q.Key == "" {
		return ErrMetricSeriesKey
	}
	if !q.End.After(q.Start) {
		return ErrMetricSeriesRange
	}
	switch q.Bucket {
	case MetricBucketNone, MetricBucketHour, MetricBucketDay, MetricBucketWeek, MetricBucketMonth:
	default:
		return ErrMetricBucket
	}
	if q.Bucket == MetricBucketNone {
		q.Aggregation = ""
		return nil
	}
	switch q.Aggregation {
	case "":
		q.Aggregation = MetricAggAvg
	case MetricAggAvg, MetricAggMin, MetricAggMax, MetricAggSum, MetricAggLast, MetricAggFirst, MetricAggCount:
	default:
		return ErrMetricAggregation
	}
	return nil
}

// Downsample keeps at most max points, evenly spaced, always including the
// last one; max <= 0 returns the input.
func Downsample(points []MetricSeriesPoint, max int) []MetricSeriesPoint {
	if max <= 0 || len(points) <= max {
		return points
	}
	if max == 1 {
		return points[len(points)-1:]
	}
	out := make([]MetricSeriesPoint, 0, max)
	step := float64(len(points)-1) / float64(max-1)
	for i := range max {
		out = append(out, points[int(float64(i)*step+0.5)])
	}
	return out
}
