package domain

import "math"

// TrainingLoadDay is one day of the training-load curves.
type TrainingLoadDay struct {
	Date string // YYYY-MM-DD, UTC
	CTL  float64
	ATL  float64
	TSB  float64
	TSS  float64
}

// TrainingLoadSeries buckets training-load metrics per UTC day, in the
// order first seen (the repo returns them chronological). Values are rounded
// to one decimal.
func TrainingLoadSeries(metrics []Metric) []TrainingLoadDay {
	byDay := map[string]*TrainingLoadDay{}
	var out []*TrainingLoadDay
	for _, m := range metrics {
		d := m.Timestamp.UTC().Format("2006-01-02")
		p, ok := byDay[d]
		if !ok {
			p = &TrainingLoadDay{Date: d}
			byDay[d] = p
			out = append(out, p)
		}
		v := 0.0
		if m.ValueNumeric != nil {
			v = *m.ValueNumeric
		}
		switch m.Key {
		case MetricKeyTrainingLoadCTL:
			p.CTL = v
		case MetricKeyTrainingLoadATL:
			p.ATL = v
		case MetricKeyTrainingLoadTSB:
			p.TSB = v
		case MetricKeyTrainingLoadTSS:
			p.TSS = v
		}
	}
	days := make([]TrainingLoadDay, 0, len(out))
	for _, p := range out {
		days = append(days, TrainingLoadDay{Date: p.Date, CTL: Round1(p.CTL), ATL: Round1(p.ATL), TSB: Round1(p.TSB), TSS: Round1(p.TSS)})
	}
	return days
}

// Round1 rounds half away from zero to one decimal.
func Round1(v float64) float64 { return math.Round(v*10) / 10 }

// BestEffortHistoryMetrics are the metrics a history can be asked for.
var BestEffortHistoryMetrics = map[string]bool{"pace": true, "speed": true, "power": true, "heart_rate": true, "vam": true}

// BestEffortHistoryBest is the all-time best of a history: the minimum for
// pace (seconds per km), the maximum otherwise; 0 for an empty history.
func BestEffortHistoryBest(items []BestEffortHistoryItem, metric string) float64 {
	best := 0.0
	for i, it := range items {
		switch {
		case i == 0:
			best = it.AchievedValue
		case metric == "pace" && it.AchievedValue < best:
			best = it.AchievedValue
		case metric != "pace" && it.AchievedValue > best:
			best = it.AchievedValue
		}
	}
	return best
}
