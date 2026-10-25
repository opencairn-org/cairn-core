package postgres

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ---------------------------------------------------------------------------
// activity_sources.parsed (jsonb)
//
// payloadJSON is the on-disk shape for domain.ActivitySourcePayload. The
// "v" field is a schema version that future migrations can bump if the
// shape needs to evolve without rewriting old rows.
// ---------------------------------------------------------------------------

const payloadSchemaVersion = 1

type payloadJSON struct {
	V int `json:"v"`

	Type          string `json:"type"`
	Discipline    string `json:"discipline,omitempty"`
	IsVirtual     bool   `json:"is_virtual,omitempty"`
	IsEbike       bool   `json:"is_ebike,omitempty"`
	IsCommute     bool   `json:"is_commute,omitempty"`
	IsRace        bool   `json:"is_race,omitempty"`
	CustomSubtype string `json:"custom_subtype,omitempty"`

	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`

	StartTime        time.Time `json:"start_time"`
	EndTime          time.Time `json:"end_time"`
	ElapsedDurationS int64     `json:"elapsed_duration_s"`
	MovingDurationS  int64     `json:"moving_duration_s"`
	Timezone         string    `json:"timezone"`

	Summary   summaryJSON `json:"summary"`
	HasStream bool        `json:"has_stream,omitempty"`
	Laps      []lapJSON   `json:"laps,omitempty"`

	GearExternalID string `json:"gear_external_id,omitempty"`
	GearName       string `json:"gear_name,omitempty"`
	GearUnknown    bool   `json:"gear_unknown,omitempty"`
}

type summaryJSON struct {
	DistanceM      *float64 `json:"distance_m,omitempty"`
	ElevationGainM *float64 `json:"elevation_gain_m,omitempty"`
	ElevationLossM *float64 `json:"elevation_loss_m,omitempty"`
	MinElevationM  *float64 `json:"min_elevation_m,omitempty"`
	MaxElevationM  *float64 `json:"max_elevation_m,omitempty"`

	AvgSpeedMps *float64 `json:"avg_speed_mps,omitempty"`
	MaxSpeedMps *float64 `json:"max_speed_mps,omitempty"`

	AvgHeartRateBpm *int `json:"avg_heart_rate_bpm,omitempty"`
	MaxHeartRateBpm *int `json:"max_heart_rate_bpm,omitempty"`

	AvgPowerW        *int `json:"avg_power_w,omitempty"`
	MaxPowerW        *int `json:"max_power_w,omitempty"`
	NormalizedPowerW *int `json:"normalized_power_w,omitempty"`

	AvgCadence *int `json:"avg_cadence,omitempty"`
	MaxCadence *int `json:"max_cadence,omitempty"`

	AvgTemperatureC *float64 `json:"avg_temperature_c,omitempty"`
	MinTemperatureC *float64 `json:"min_temperature_c,omitempty"`
	MaxTemperatureC *float64 `json:"max_temperature_c,omitempty"`

	CaloriesKcal    *int     `json:"calories_kcal,omitempty"`
	TSS             *float64 `json:"tss,omitempty"`
	IntensityFactor *float64 `json:"intensity_factor,omitempty"`

	PoolLengthM  *int `json:"pool_length_m,omitempty"`
	TotalStrokes *int `json:"total_strokes,omitempty"`
}

type lapJSON struct {
	Index            int      `json:"index,omitempty"`
	Label            string   `json:"label,omitempty"`
	StartOffsetS     int64    `json:"start_offset_s"`
	ElapsedDurationS int64    `json:"elapsed_duration_s"`
	MovingDurationS  int64    `json:"moving_duration_s"`
	DistanceM        *float64 `json:"distance_m,omitempty"`
	AvgSpeedMps      *float64 `json:"avg_speed_mps,omitempty"`
	AvgHeartRateBpm  *int     `json:"avg_heart_rate_bpm,omitempty"`
	AvgPowerW        *int     `json:"avg_power_w,omitempty"`
	AvgCadence       *int     `json:"avg_cadence,omitempty"`
	ElevationGainM   *float64 `json:"elevation_gain_m,omitempty"`
	ElevationLossM   *float64 `json:"elevation_loss_m,omitempty"`
}

// encodePayload marshals a domain payload into the on-disk JSON shape.
func encodePayload(p domain.ActivitySourcePayload) ([]byte, error) {
	w := payloadJSON{
		V:                payloadSchemaVersion,
		Type:             string(p.Type),
		Discipline:       string(p.Discipline),
		IsVirtual:        p.IsVirtual,
		IsEbike:          p.IsEbike,
		IsCommute:        p.IsCommute,
		IsRace:           p.IsRace,
		CustomSubtype:    p.CustomSubtype,
		Title:            p.Title,
		Description:      p.Description,
		StartTime:        p.StartTime,
		EndTime:          p.EndTime,
		ElapsedDurationS: int64(p.ElapsedDuration / time.Second),
		MovingDurationS:  int64(p.MovingDuration / time.Second),
		Timezone:         p.Timezone,
		Summary:          payloadSummaryToJSON(p.Summary),
		HasStream:        p.HasStream,
		GearExternalID:   p.GearExternalID,
		GearName:         p.GearName,
		GearUnknown:      p.GearUnknown,
	}
	w.Laps = payloadLapsToJSON(p.Laps)
	return json.Marshal(w)
}

// decodePayload unmarshals an on-disk row into a domain payload.
func decodePayload(b []byte) (domain.ActivitySourcePayload, error) {
	if len(b) == 0 {
		return domain.ActivitySourcePayload{}, fmt.Errorf("empty payload jsonb")
	}
	var w payloadJSON
	if err := json.Unmarshal(b, &w); err != nil {
		return domain.ActivitySourcePayload{}, fmt.Errorf("unmarshal payload: %w", err)
	}
	if w.V != 0 && w.V != payloadSchemaVersion {
		return domain.ActivitySourcePayload{}, fmt.Errorf("payload schema version %d not supported (this binary expects %d)", w.V, payloadSchemaVersion)
	}

	p := domain.ActivitySourcePayload{
		Type:            domain.ActivityType(w.Type),
		Discipline:      domain.Discipline(w.Discipline),
		IsVirtual:       w.IsVirtual,
		IsEbike:         w.IsEbike,
		IsCommute:       w.IsCommute,
		IsRace:          w.IsRace,
		CustomSubtype:   w.CustomSubtype,
		Title:           w.Title,
		Description:     w.Description,
		StartTime:       w.StartTime,
		EndTime:         w.EndTime,
		ElapsedDuration: time.Duration(w.ElapsedDurationS) * time.Second,
		MovingDuration:  time.Duration(w.MovingDurationS) * time.Second,
		Timezone:        w.Timezone,
		Summary:         payloadSummaryFromJSON(w.Summary),
		HasStream:       w.HasStream,
		GearExternalID:  w.GearExternalID,
		GearName:        w.GearName,
		GearUnknown:     w.GearUnknown,
	}
	p.Laps = payloadLapsFromJSON(w.Laps)
	return p, nil
}

// payloadSummaryToJSON maps a domain summary onto its wire shape.
func payloadSummaryToJSON(s domain.ActivitySummary) summaryJSON {
	return summaryJSON{
		DistanceM:        s.DistanceM,
		ElevationGainM:   s.ElevationGainM,
		ElevationLossM:   s.ElevationLossM,
		MinElevationM:    s.MinElevationM,
		MaxElevationM:    s.MaxElevationM,
		AvgSpeedMps:      s.AvgSpeedMps,
		MaxSpeedMps:      s.MaxSpeedMps,
		AvgHeartRateBpm:  s.AvgHeartRateBpm,
		MaxHeartRateBpm:  s.MaxHeartRateBpm,
		AvgPowerW:        s.AvgPowerW,
		MaxPowerW:        s.MaxPowerW,
		NormalizedPowerW: s.NormalizedPowerW,
		AvgCadence:       s.AvgCadence,
		MaxCadence:       s.MaxCadence,
		AvgTemperatureC:  s.AvgTemperatureC,
		MinTemperatureC:  s.MinTemperatureC,
		MaxTemperatureC:  s.MaxTemperatureC,
		CaloriesKcal:     s.CaloriesKcal,
		TSS:              s.TSS,
		IntensityFactor:  s.IntensityFactor,
		PoolLengthM:      s.PoolLengthM,
		TotalStrokes:     s.TotalStrokes,
	}
}

// payloadSummaryFromJSON maps a wire summary onto the domain shape.
func payloadSummaryFromJSON(s summaryJSON) domain.ActivitySummary {
	return domain.ActivitySummary{
		DistanceM:        s.DistanceM,
		ElevationGainM:   s.ElevationGainM,
		ElevationLossM:   s.ElevationLossM,
		MinElevationM:    s.MinElevationM,
		MaxElevationM:    s.MaxElevationM,
		AvgSpeedMps:      s.AvgSpeedMps,
		MaxSpeedMps:      s.MaxSpeedMps,
		AvgHeartRateBpm:  s.AvgHeartRateBpm,
		MaxHeartRateBpm:  s.MaxHeartRateBpm,
		AvgPowerW:        s.AvgPowerW,
		MaxPowerW:        s.MaxPowerW,
		NormalizedPowerW: s.NormalizedPowerW,
		AvgCadence:       s.AvgCadence,
		MaxCadence:       s.MaxCadence,
		AvgTemperatureC:  s.AvgTemperatureC,
		MinTemperatureC:  s.MinTemperatureC,
		MaxTemperatureC:  s.MaxTemperatureC,
		CaloriesKcal:     s.CaloriesKcal,
		TSS:              s.TSS,
		IntensityFactor:  s.IntensityFactor,
		PoolLengthM:      s.PoolLengthM,
		TotalStrokes:     s.TotalStrokes,
	}
}

// payloadLapsToJSON maps domain laps onto their wire shape; nil when empty.
func payloadLapsToJSON(laps []domain.ActivityLap) []lapJSON {
	var out []lapJSON
	if len(laps) > 0 {
		out = make([]lapJSON, len(laps))
		for i, l := range laps {
			out[i] = lapJSON{
				Index:            l.Index,
				Label:            l.Label,
				StartOffsetS:     int64(l.StartOffset / time.Second),
				ElapsedDurationS: int64(l.ElapsedDuration / time.Second),
				MovingDurationS:  int64(l.MovingDuration / time.Second),
				DistanceM:        l.DistanceM,
				AvgSpeedMps:      l.AvgSpeedMps,
				AvgHeartRateBpm:  l.AvgHeartRateBpm,
				AvgPowerW:        l.AvgPowerW,
				AvgCadence:       l.AvgCadence,
				ElevationGainM:   l.ElevationGainM,
				ElevationLossM:   l.ElevationLossM,
			}
		}
	}
	return out
}

// payloadLapsFromJSON maps wire laps onto the domain shape; nil when empty.
func payloadLapsFromJSON(laps []lapJSON) []domain.ActivityLap {
	var out []domain.ActivityLap
	if len(laps) > 0 {
		out = make([]domain.ActivityLap, len(laps))
		for i, l := range laps {
			out[i] = domain.ActivityLap{
				Index:           l.Index,
				Label:           l.Label,
				StartOffset:     time.Duration(l.StartOffsetS) * time.Second,
				ElapsedDuration: time.Duration(l.ElapsedDurationS) * time.Second,
				MovingDuration:  time.Duration(l.MovingDurationS) * time.Second,
				DistanceM:       l.DistanceM,
				AvgSpeedMps:     l.AvgSpeedMps,
				AvgHeartRateBpm: l.AvgHeartRateBpm,
				AvgPowerW:       l.AvgPowerW,
				AvgCadence:      l.AvgCadence,
				ElevationGainM:  l.ElevationGainM,
				ElevationLossM:  l.ElevationLossM,
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// activities.merge_provenance (jsonb)
//
// Encoded as a flat string map: FieldGroup -> UUID string. Empty
// provenance encodes as "{}".
// ---------------------------------------------------------------------------

// encodeProvenance turns a domain MergeProvenance into JSON bytes.
func encodeProvenance(m domain.MergeProvenance) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[string(k)] = v.UUID().String()
	}
	return json.Marshal(out)
}

// decodeProvenance turns JSON bytes from the DB into a domain MergeProvenance.
// Empty / null jsonb is tolerated and returns an empty map.
func decodeProvenance(b []byte) (domain.MergeProvenance, error) {
	if len(b) == 0 || string(b) == "null" {
		return domain.MergeProvenance{}, nil
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal merge_provenance: %w", err)
	}
	out := make(domain.MergeProvenance, len(raw))
	for k, v := range raw {
		id, err := uuid.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("merge_provenance[%s] not a uuid: %w", k, err)
		}
		out[domain.FieldGroup(k)] = domain.SourceID(id)
	}
	return out, nil
}
