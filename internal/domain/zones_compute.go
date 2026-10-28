package domain

import "math"

// ZoneBucket is one zone band with the seconds spent in it.
type ZoneBucket struct {
	Label   string
	LowPct  float64
	HighPct float64
	Seconds int
}

// ZoneSummary is time-in-zone for one metric against a reference value.
type ZoneSummary struct {
	Basis        string // "lthr" | "max" | "ftp"
	Reference    float64
	TotalSeconds int
	Zones        []ZoneBucket
}

// Decoupling is the aerobic decoupling of a steady effort.
type Decoupling struct {
	Pct     float64 // rounded to one decimal
	Basis   string  // "power" | "pace"
	Coupled bool    // < 5 %
}

// ZonesResult is the per-activity time-in-zone view; nil parts are unavailable.
type ZonesResult struct {
	HR         *ZoneSummary
	Power      *ZoneSummary
	Decoupling *Decoupling
}

// ThresholdAt resolves an athlete metric at the activity's date.
type ThresholdAt func(k AthleteMetricKey) (float64, bool)

// zonesMaxGap caps the seconds a single sample can contribute, so recording
// pauses (large timestamp gaps) don't inflate a zone.
const zonesMaxGap = 10.0

// decouplingMinSamples is the paired-sample floor (≈10 min at 1 Hz) for a
// decoupling figure to mean anything.
const decouplingMinSamples = 600

// ComputeZones derives heart-rate and power time-in-zone and aerobic
// decoupling from raw samples and the athlete's thresholds as of the
// activity's date. HR zones prefer LTHR and fall back to max HR; power zones
// need FTP. Decoupling is only reported for a sustained, steady effort
// (≥ decouplingMinSamples paired samples, |pct| ≤ 30), power-paired first,
// else speed-paired over genuinely moving samples.
func ComputeZones(samples []StreamSample, valAt ThresholdAt) ZonesResult {
	z := collectZoneSeries(samples)
	var out ZonesResult
	if len(z.hrVals) > 0 {
		if ref, basis, ok := hrReference(valAt); ok {
			bands := HRZoneBands(basis)
			out.HR = zoneSummary(basis, ref, bands, BucketSeconds(bands, ref, z.hrVals, z.hrDts))
		}
	}
	if len(z.pwVals) > 0 {
		if ftp, ok := valAt(AthleteFTPWatts); ok && ftp > 0 {
			bands := PowerZoneBands()
			out.Power = zoneSummary("ftp", ftp, bands, BucketSeconds(bands, ftp, z.pwVals, z.pwDts))
		}
	}
	if d, ok := decoupling(z.dHRp, z.dOutp, "power"); ok {
		out.Decoupling = &d
	} else if d, ok := decoupling(z.dHRs, z.dOuts, "pace"); ok {
		out.Decoupling = &d
	}
	return out
}

func hrReference(valAt ThresholdAt) (ref float64, basis string, ok bool) {
	if v, ok := valAt(AthleteThresholdHR); ok && v > 0 {
		return v, "lthr", true
	}
	if v, ok := valAt(AthleteMaxHR); ok && v > 0 {
		return v, "max", true
	}
	return 0, "", false
}

func zoneSummary(basis string, ref float64, bands []ZoneBand, secs []float64) *ZoneSummary {
	s := &ZoneSummary{Basis: basis, Reference: ref, Zones: make([]ZoneBucket, len(bands))}
	total := 0.0
	for i, b := range bands {
		total += secs[i]
		s.Zones[i] = ZoneBucket{Label: b.Label, LowPct: b.LowPct, HighPct: b.HighPct, Seconds: int(secs[i] + 0.5)}
	}
	s.TotalSeconds = int(total + 0.5)
	return s
}

func decoupling(hr, out []float64, basis string) (Decoupling, bool) {
	if len(hr) < decouplingMinSamples {
		return Decoupling{}, false
	}
	d, ok := AerobicDecoupling(hr, out)
	if !ok || d < -30 || d > 30 {
		return Decoupling{}, false
	}
	return Decoupling{Pct: math.Round(d*10) / 10, Basis: basis, Coupled: d < 5}, true
}

// zoneSeries holds the per-sample value/dt series derived from a stream.
type zoneSeries struct {
	hrVals, hrDts, pwVals, pwDts []float64
	dHRp, dOutp, dHRs, dOuts     []float64
}

func zoneSampleDt(samples []StreamSample, i int) float64 {
	if i+1 >= len(samples) {
		return 1.0
	}
	dt := samples[i+1].Timestamp.Sub(samples[i].Timestamp).Seconds()
	if dt <= 0 || dt > zonesMaxGap {
		return 1.0
	}
	return dt
}

func collectZoneSeries(samples []StreamSample) zoneSeries {
	var z zoneSeries
	for i, s := range samples {
		dt := zoneSampleDt(samples, i)
		hr := 0.0
		if s.HeartRateBpm != nil && *s.HeartRateBpm > 0 {
			hr = float64(*s.HeartRateBpm)
			z.hrVals = append(z.hrVals, hr)
			z.hrDts = append(z.hrDts, dt)
		}
		if s.PowerW != nil && *s.PowerW > 0 {
			z.pwVals = append(z.pwVals, float64(*s.PowerW))
			z.pwDts = append(z.pwDts, dt)
			if hr > 0 {
				z.dHRp = append(z.dHRp, hr)
				z.dOutp = append(z.dOutp, float64(*s.PowerW))
			}
		}
		// Pace decoupling only counts genuinely moving samples (>1 m/s).
		if hr > 0 && s.SpeedMps != nil && *s.SpeedMps > 1.0 {
			z.dHRs = append(z.dHRs, hr)
			z.dOuts = append(z.dOuts, *s.SpeedMps)
		}
	}
	return z
}
