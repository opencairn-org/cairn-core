package domain

import (
	"fmt"
	"strconv"
	"time"
)

// SourcePreviewStatus is the lifecycle of a reimport preview.
type SourcePreviewStatus string

const (
	SourcePreviewPending SourcePreviewStatus = "pending" // waiting for the worker result
	SourcePreviewReady   SourcePreviewStatus = "ready"   // diffs computed, commit or discard
)

// SourcePreviewTTL bounds how long a preview may be committed.
const SourcePreviewTTL = time.Hour

// FieldChange is one before/after pair of a diff, rendered as strings so
// clients can show it without knowing the field's type.
type FieldChange struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// SourcePreview is a dry-run reimport: the fresh payload a worker produced
// for a source and what committing it would change, held for SourcePreviewTTL.
type SourcePreview struct {
	ID         SourcePreviewID
	SourceID   SourceID
	ActivityID ActivityID
	UserID     UserID
	JobID      string
	Status     SourcePreviewStatus
	// Parsed is the proposed worker result as stored (opaque to the domain).
	Parsed     []byte
	SourceDiff []FieldChange
	MergedDiff []FieldChange
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// Expired reports whether the preview can no longer be committed.
func (p SourcePreview) Expired(now time.Time) bool { return !now.Before(p.ExpiresAt) }

// DiffSourcePayloads lists the fields that differ between two parsed payloads.
func DiffSourcePayloads(before, after ActivitySourcePayload) []FieldChange {
	d := &differ{}
	d.str("type", string(before.Type), string(after.Type))
	d.str("discipline", string(before.Discipline), string(after.Discipline))
	d.bool("is_virtual", before.IsVirtual, after.IsVirtual)
	d.bool("is_ebike", before.IsEbike, after.IsEbike)
	d.bool("is_commute", before.IsCommute, after.IsCommute)
	d.bool("is_race", before.IsRace, after.IsRace)
	d.str("title", before.Title, after.Title)
	d.str("description", before.Description, after.Description)
	d.time("start_time", before.StartTime, after.StartTime)
	d.dur("elapsed_duration", before.ElapsedDuration, after.ElapsedDuration)
	d.dur("moving_duration", before.MovingDuration, after.MovingDuration)
	d.str("timezone", before.Timezone, after.Timezone)
	d.summary(before.Summary, after.Summary)
	d.bool("has_stream", before.HasStream, after.HasStream)
	d.int("laps", len(before.Laps), len(after.Laps))
	d.str("gear_external_id", before.GearExternalID, after.GearExternalID)
	return d.out
}

// DiffActivities lists the merged-activity fields a commit would change.
func DiffActivities(before, after Activity) []FieldChange {
	d := &differ{}
	d.str("type", string(before.Type), string(after.Type))
	d.str("discipline", string(before.Discipline), string(after.Discipline))
	d.bool("is_virtual", before.IsVirtual, after.IsVirtual)
	d.bool("is_ebike", before.IsEbike, after.IsEbike)
	d.bool("is_commute", before.IsCommute, after.IsCommute)
	d.bool("is_race", before.IsRace, after.IsRace)
	d.str("title", before.Title, after.Title)
	d.str("description", before.Description, after.Description)
	d.time("start_time", before.StartTime, after.StartTime)
	d.dur("elapsed_duration", before.ElapsedDuration, after.ElapsedDuration)
	d.dur("moving_duration", before.MovingDuration, after.MovingDuration)
	d.str("timezone", before.Timezone, after.Timezone)
	d.summary(before.Summary, after.Summary)
	return d.out
}

type differ struct{ out []FieldChange }

func (d *differ) add(field, before, after string) {
	if before != after {
		d.out = append(d.out, FieldChange{Field: field, Before: before, After: after})
	}
}
func (d *differ) str(f, a, b string) { d.add(f, a, b) }
func (d *differ) bool(f string, a, b bool) {
	d.add(f, strconv.FormatBool(a), strconv.FormatBool(b))
}
func (d *differ) int(f string, a, b int) { d.add(f, strconv.Itoa(a), strconv.Itoa(b)) }
func (d *differ) time(f string, a, b time.Time) {
	if !a.Equal(b) {
		d.add(f, fmtTime(a), fmtTime(b))
	}
}
func (d *differ) dur(f string, a, b time.Duration) {
	d.add(f, strconv.FormatInt(int64(a.Seconds()), 10), strconv.FormatInt(int64(b.Seconds()), 10))
}
func (d *differ) f64(f string, a, b *float64) { d.add(f, fmtF64(a), fmtF64(b)) }
func (d *differ) intp(f string, a, b *int)    { d.add(f, fmtInt(a), fmtInt(b)) }

func (d *differ) summary(a, b ActivitySummary) {
	d.f64("distance_m", a.DistanceM, b.DistanceM)
	d.f64("elevation_gain_m", a.ElevationGainM, b.ElevationGainM)
	d.f64("elevation_loss_m", a.ElevationLossM, b.ElevationLossM)
	d.f64("avg_speed_mps", a.AvgSpeedMps, b.AvgSpeedMps)
	d.f64("max_speed_mps", a.MaxSpeedMps, b.MaxSpeedMps)
	d.intp("avg_heart_rate_bpm", a.AvgHeartRateBpm, b.AvgHeartRateBpm)
	d.intp("max_heart_rate_bpm", a.MaxHeartRateBpm, b.MaxHeartRateBpm)
	d.intp("avg_power_w", a.AvgPowerW, b.AvgPowerW)
	d.intp("max_power_w", a.MaxPowerW, b.MaxPowerW)
	d.intp("avg_cadence", a.AvgCadence, b.AvgCadence)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
func fmtF64(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', 1, 64)
}
func fmtInt(v *int) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(*v)
}
