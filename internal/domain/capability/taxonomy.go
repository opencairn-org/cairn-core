// Package capability defines the provider-neutral data-type taxonomy and the
// per-type capability model (read/write/backfill) a worker advertises.
// Pure domain: providers map their schema onto these types; the core queries
// capabilities generically.
package capability

// DataType is a provider-neutral data category. String values are the stable
// wire identifiers (manifests, heartbeat, proto) — keep them stable.
type DataType string

// Activity-related types. PersonalBest (not "best effort", which means delivery)
// is a provider-reported best like "fastest 5k".
const (
	DataTypeActivity      DataType = "Activity"
	DataTypeLap           DataType = "Lap"
	DataTypeSegmentEffort DataType = "SegmentEffort"
	DataTypePersonalBest  DataType = "PersonalBest"
)

// Time-series / health metrics.
const (
	DataTypeSteps       DataType = "Steps"
	DataTypeHRV         DataType = "HRV"
	DataTypeRestingHR   DataType = "RestingHR"
	DataTypeSleep       DataType = "Sleep"
	DataTypeWeight      DataType = "Weight"
	DataTypeWaterIntake DataType = "WaterIntake"
)

// Static / reference types. Segment is the definition, distinct from SegmentEffort.
const (
	DataTypeSegment        DataType = "Segment"
	DataTypeGear           DataType = "Gear"
	DataTypeAthleteProfile DataType = "AthleteProfile"
)

// Category groups data types for display and routing.
type Category string

const (
	CategoryActivity   Category = "activity"
	CategoryTimeSeries Category = "timeseries"
	CategoryStatic     Category = "static"
)

// TypeSpec is the registry metadata for a data type: a human label and the
// category it belongs to.
type TypeSpec struct {
	Type     DataType
	Label    string
	Category Category
}

// registry is the closed set of known data types in a stable display order.
var registry = []TypeSpec{
	{DataTypeActivity, "Activity", CategoryActivity},
	{DataTypeLap, "Laps", CategoryActivity},
	{DataTypeSegmentEffort, "Segment Effort", CategoryActivity},
	{DataTypePersonalBest, "Personal Best", CategoryActivity},

	{DataTypeSteps, "Steps", CategoryTimeSeries},
	{DataTypeHRV, "HRV", CategoryTimeSeries},
	{DataTypeRestingHR, "Resting HR", CategoryTimeSeries},
	{DataTypeSleep, "Sleep", CategoryTimeSeries},
	{DataTypeWeight, "Weight", CategoryTimeSeries},
	{DataTypeWaterIntake, "Water Intake", CategoryTimeSeries},

	{DataTypeSegment, "Segment", CategoryStatic},
	{DataTypeGear, "Gear", CategoryStatic},
	{DataTypeAthleteProfile, "Athlete Profile", CategoryStatic},
}

// byType indexes the registry for O(1) validity/label lookups.
var byType = func() map[DataType]TypeSpec {
	m := make(map[DataType]TypeSpec, len(registry))
	for _, s := range registry {
		m[s.Type] = s
	}
	return m
}()

// MetricTypes returns the time-series (health metric) data types, the ones an
// import_metrics job delivers.
func MetricTypes() []DataType {
	var out []DataType
	for _, s := range registry {
		if s.Category == CategoryTimeSeries {
			out = append(out, s.Type)
		}
	}
	return out
}

// AllTypes returns the canonical data types in display order.
func AllTypes() []TypeSpec {
	out := make([]TypeSpec, len(registry))
	copy(out, registry)
	return out
}

// Valid reports whether dt is a known canonical data type.
func (dt DataType) Valid() bool {
	_, ok := byType[dt]
	return ok
}

// Label returns the human label for dt, or the raw identifier if unknown.
func (dt DataType) Label() string {
	if s, ok := byType[dt]; ok {
		return s.Label
	}
	return string(dt)
}

// CategoryOf returns dt's category, or "" if unknown.
func (dt DataType) CategoryOf() Category {
	if s, ok := byType[dt]; ok {
		return s.Category
	}
	return ""
}
