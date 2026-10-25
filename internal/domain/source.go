package domain

import "time"

// ---------------------------------------------------------------------------
// ActivitySource
//
// One row per (provider, external_account, external_id) tuple. Many sources
// can attach to one Activity; the merge engine reads them and produces the
// canonical Activity view.
// ---------------------------------------------------------------------------

type ActivitySource struct {
	ID         SourceID
	ActivityID ActivityID
	UserID     UserID

	// Provider identification
	Provider          string             // any provider identifier, e.g. external system name or "manual_upload"
	ExternalAccountID *ExternalAccountID // nil for manual uploads
	ExternalID        string             // provider-side ID; unique per (provider, account)

	// Worker provenance — versioning lets the auto-reimport policy detect
	// when a source can be re-parsed by a newer worker.
	SourceWorkerName    string
	SourceWorkerVersion string // parser version "N.M" (docs/adr/0005)
	SourceWorkerPackage string
	SourceWorkerRelease string // release tag that last parsed it; diagnostics only

	// Raw blob reference (S3 key). Optional — some workers stream-parse
	// without storing a raw blob.
	RawBlobID      string
	RawContentType string
	RawSizeBytes   int64

	// The parsed payload — the merge engine's only input besides priority.
	Parsed ActivitySourcePayload

	Status       SourceStatus
	StatusReason string

	ReimportStatus       ReimportStatus
	ReimportStatusReason string

	ImportedAt       time.Time
	LastReimportedAt *time.Time
	UpdatedAt        time.Time

	// StartLat/StartLng: start coordinate from the stream's first GPS sample;
	// nil for indoor/no-GPS. A matcher tiebreaker, denormalized for blocking.
	StartLat *float64
	StartLng *float64
}

// SourceMatchRecord is the blocking/scoring view of a source the matcher
// consumes (denormalized features + identity). Times are UTC.
type SourceMatchRecord struct {
	SourceID          SourceID
	ActivityID        ActivityID
	UserID            UserID
	Provider          string
	ExternalAccountID *ExternalAccountID
	ExternalID        string
	SportClass        string
	StartUTC          time.Time
	DistanceM         *float64
	MovingS           int64
	ElapsedS          int64
	StartLat          *float64
	StartLng          *float64
	Status            SourceStatus
}

// ---------------------------------------------------------------------------
// Status enums (mirror DB CHECK constraints in 00005_activities.sql)
// ---------------------------------------------------------------------------

type SourceStatus string

const (
	SourceStatusActive             SourceStatus = "active"
	SourceStatusOrphaned           SourceStatus = "orphaned"
	SourceStatusAccountUnavailable SourceStatus = "account_unavailable"
	SourceStatusDetached           SourceStatus = "detached"
)

type ReimportStatus string

const (
	ReimportStatusCurrent         ReimportStatus = "current"
	ReimportStatusUpdateAvailable ReimportStatus = "update_available"
	ReimportStatusUpdating        ReimportStatus = "updating"
	ReimportStatusFailed          ReimportStatus = "failed"
)

// ---------------------------------------------------------------------------
// ActivitySourcePayload
//
// The structured form of what a worker parsed out of provider data. Mirrors
// cairn.worker.v1.ActivitySourcePayload but lives in the domain package so
// the merge engine has no proto dependency.
//
// Pointers signal "field absent" vs "field zero". Required-for-the-merge
// fields (StartTime, ElapsedDuration) are non-nullable; the merge engine
// presumes every source defines a time bound.
// ---------------------------------------------------------------------------

type ActivitySourcePayload struct {
	// Classification — present in almost every source.
	Type          ActivityType
	Discipline    Discipline
	IsVirtual     bool
	IsEbike       bool
	IsCommute     bool
	IsRace        bool
	CustomSubtype string

	Title       string
	Description string

	// Time bounds
	StartTime       time.Time
	EndTime         time.Time
	ElapsedDuration time.Duration
	MovingDuration  time.Duration
	Timezone        string

	// Summary metrics — same shape as ActivitySummary but per-source.
	Summary ActivitySummary

	// Whether this source carries a stream (1Hz samples) the user can
	// view as a chart. Workers set this true even when they haven't
	// uploaded the stream rows yet — the rows are inserted by the
	// PersistActivityStream worker job after the source is committed.
	HasStream bool

	// Lap segmentation, if the source provides it. Empty when unavailable.
	Laps []ActivityLap

	// Provider gear id on this activity (resolved to a Gear through the
	// account's gear refs) and the provider's display name as a hint until
	// the gear is imported.
	GearExternalID string
	GearName       string
}

// ActivityLap is a per-lap summary record. Same per-source flag and merge
// rules: the merge engine takes the laps from the winning source for the
// "laps" field group (currently a single source wins; we don't merge laps
// across sources because their boundaries rarely align).
type ActivityLap struct {
	Index           int    // provider lap index (0-based)
	Label           string // provider-supplied lap name, when any
	StartOffset     time.Duration
	ElapsedDuration time.Duration
	MovingDuration  time.Duration
	DistanceM       *float64
	AvgSpeedMps     *float64
	AvgHeartRateBpm *int
	AvgPowerW       *int
	AvgCadence      *int
	ElevationGainM  *float64
	ElevationLossM  *float64
}

// ---------------------------------------------------------------------------
// Field-group probes
//
// The merge engine asks every source "do you provide field group X?" and
// then "apply field group X to this target Activity". These methods are
// the source's per-group view of its own data.
//
// The split between "Provides" and "Apply" lets the engine pick the winner
// per group cheaply (a single probe per source per group) before paying
// for the apply cost on only the winner.
// ---------------------------------------------------------------------------

// Provides reports whether the payload carries a non-null value for the
// given field group. Used by the merge engine to pick the winning source.
func (p *ActivitySourcePayload) Provides(g FieldGroup) bool {
	switch g {
	case FieldGroupClassification:
		return p.Type.Valid()

	case FieldGroupTitle:
		return p.Title != ""

	case FieldGroupDescription:
		return p.Description != ""

	case FieldGroupDuration:
		return p.ElapsedDuration > 0

	case FieldGroupTime:
		return !p.StartTime.IsZero()

	case FieldGroupGPSTrack:
		return p.HasStream

	case FieldGroupLaps:
		return len(p.Laps) > 0

	case FieldGroupGear:
		return p.GearExternalID != ""
	}
	return p.providesSummary(g)
}

// providesSummary reports whether any field of a summary-metric group is set.
func (p *ActivitySourcePayload) providesSummary(g FieldGroup) bool {
	switch g {
	case FieldGroupDistance:
		return p.Summary.DistanceM != nil

	case FieldGroupElevation:
		return p.Summary.ElevationGainM != nil ||
			p.Summary.ElevationLossM != nil ||
			p.Summary.MinElevationM != nil ||
			p.Summary.MaxElevationM != nil

	case FieldGroupSpeed:
		return p.Summary.AvgSpeedMps != nil || p.Summary.MaxSpeedMps != nil

	case FieldGroupHeartRate:
		return p.Summary.AvgHeartRateBpm != nil || p.Summary.MaxHeartRateBpm != nil

	case FieldGroupPower:
		return p.Summary.AvgPowerW != nil ||
			p.Summary.MaxPowerW != nil ||
			p.Summary.NormalizedPowerW != nil

	case FieldGroupCadence:
		return p.Summary.AvgCadence != nil || p.Summary.MaxCadence != nil

	case FieldGroupTemperature:
		return p.Summary.AvgTemperatureC != nil ||
			p.Summary.MinTemperatureC != nil ||
			p.Summary.MaxTemperatureC != nil

	case FieldGroupCalories:
		return p.Summary.CaloriesKcal != nil

	case FieldGroupTrainingLoad:
		return p.Summary.TSS != nil || p.Summary.IntensityFactor != nil

	case FieldGroupSwim:
		return p.Summary.PoolLengthM != nil || p.Summary.TotalStrokes != nil
	}
	return false
}

// ApplyTo copies the values for the named field group from p into target.
// The merge engine calls this for the winning source per group only.
//
// ApplyTo is idempotent: calling it twice with the same source produces
// the same target. It overwrites whatever is on target, so callers must
// invoke it in priority order (winner first), then never replace with a
// lower-priority source.
func (p *ActivitySourcePayload) ApplyTo(g FieldGroup, target *Activity) {
	switch g {
	case FieldGroupClassification:
		target.Type = p.Type
		target.Discipline = p.Discipline
		target.IsVirtual = p.IsVirtual
		target.IsEbike = p.IsEbike
		target.IsCommute = p.IsCommute
		target.IsRace = p.IsRace
		target.CustomSubtype = p.CustomSubtype

	case FieldGroupTitle:
		target.Title = p.Title

	case FieldGroupDescription:
		target.Description = p.Description

	case FieldGroupTime:
		target.StartTime = p.StartTime
		target.EndTime = p.EndTime
		target.Timezone = p.Timezone

	case FieldGroupDuration:
		target.ElapsedDuration = p.ElapsedDuration
		target.MovingDuration = p.MovingDuration

	default:
		p.applySummaryTo(g, &target.Summary)

		// FieldGroupGPSTrack and FieldGroupLaps don't write fields directly on
		// the Activity — they pick the PrimaryStreamSourceID (handled in the
		// merge engine) and influence which source's laps render.
	}
}

// applySummaryTo copies the summary-metric field groups; other groups are no-ops.
func (p *ActivitySourcePayload) applySummaryTo(g FieldGroup, sum *ActivitySummary) {
	switch g {
	case FieldGroupDistance:
		sum.DistanceM = p.Summary.DistanceM

	case FieldGroupElevation:
		sum.ElevationGainM = p.Summary.ElevationGainM
		sum.ElevationLossM = p.Summary.ElevationLossM
		sum.MinElevationM = p.Summary.MinElevationM
		sum.MaxElevationM = p.Summary.MaxElevationM

	case FieldGroupSpeed:
		sum.AvgSpeedMps = p.Summary.AvgSpeedMps
		sum.MaxSpeedMps = p.Summary.MaxSpeedMps

	case FieldGroupHeartRate:
		sum.AvgHeartRateBpm = p.Summary.AvgHeartRateBpm
		sum.MaxHeartRateBpm = p.Summary.MaxHeartRateBpm

	case FieldGroupPower:
		sum.AvgPowerW = p.Summary.AvgPowerW
		sum.MaxPowerW = p.Summary.MaxPowerW
		sum.NormalizedPowerW = p.Summary.NormalizedPowerW

	case FieldGroupCadence:
		sum.AvgCadence = p.Summary.AvgCadence
		sum.MaxCadence = p.Summary.MaxCadence

	case FieldGroupTemperature:
		sum.AvgTemperatureC = p.Summary.AvgTemperatureC
		sum.MinTemperatureC = p.Summary.MinTemperatureC
		sum.MaxTemperatureC = p.Summary.MaxTemperatureC

	case FieldGroupCalories:
		sum.CaloriesKcal = p.Summary.CaloriesKcal

	case FieldGroupTrainingLoad:
		sum.TSS = p.Summary.TSS
		sum.IntensityFactor = p.Summary.IntensityFactor

	case FieldGroupSwim:
		sum.PoolLengthM = p.Summary.PoolLengthM
		sum.TotalStrokes = p.Summary.TotalStrokes
	}
}
