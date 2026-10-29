package domain

import (
	"errors"
	"strings"
	"time"
)

// CustomBestEffort is a user-defined best-effort window ("my 7.5 km loop",
// "90-minute power") computed next to the standard set for one activity type.
type CustomBestEffort struct {
	ID           CustomBestEffortID
	UserID       UserID
	ActivityType ActivityType
	Metric       BestEffortMetric
	WindowKind   BestEffortWindowKind
	WindowValue  int // metres for distance windows, seconds for duration windows
	Label        string
	CreatedAt    time.Time
}

// Bounds of a custom window; the compute scans ~1 Hz samples, so anything
// shorter than a few seconds is noise and anything longer than a day is
// never covered by one activity.
const (
	CustomWindowMinDistanceM = 100
	CustomWindowMaxDistanceM = 500_000
	CustomWindowMinDurationS = 5
	CustomWindowMaxDurationS = 24 * 3600
	MaxCustomBestEfforts     = 20
)

var (
	ErrCustomEffortLabelRequired = errors.New("label required")
	ErrCustomEffortMetricWindow  = errors.New("distance windows take the pace metric, duration windows take speed, power, heart_rate or vam")
	ErrCustomEffortWindowRange   = errors.New("window value out of range")
	ErrCustomEffortLimit         = errors.New("custom best-effort limit reached")
	ErrCustomEffortDuplicate     = errors.New("a custom best effort with this window already exists")
)

// Validate normalises the label and checks the metric/window combination
// against what the compute can evaluate.
func (c *CustomBestEffort) Validate() error {
	c.Label = strings.TrimSpace(c.Label)
	if c.Label == "" {
		return ErrCustomEffortLabelRequired
	}
	if !c.ActivityType.Valid() {
		return errors.New("invalid activity type")
	}
	switch c.WindowKind {
	case BestEffortWindowDistance:
		if c.Metric != BestEffortMetricPace {
			return ErrCustomEffortMetricWindow
		}
		if c.WindowValue < CustomWindowMinDistanceM || c.WindowValue > CustomWindowMaxDistanceM {
			return ErrCustomEffortWindowRange
		}
	case BestEffortWindowDuration:
		switch c.Metric {
		case BestEffortMetricSpeed, BestEffortMetricPower, BestEffortMetricHeartRate, BestEffortMetricVAM:
		default:
			return ErrCustomEffortMetricWindow
		}
		if c.WindowValue < CustomWindowMinDurationS || c.WindowValue > CustomWindowMaxDurationS {
			return ErrCustomEffortWindowRange
		}
	default:
		return ErrCustomEffortMetricWindow
	}
	return nil
}
