// Package health models non-activity health time-series (HRV, Sleep, Weight,
// Steps, WaterIntake, RestingHR) and their per-day, per-type merge across
// providers. These bypass the activity matcher but use the same provider
// cascade. Pure domain.
package health

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
)

// Sample is one health reading from one provider.
type Sample struct {
	UserID      domain.UserID
	DataType    capability.DataType
	Timestamp   time.Time
	Provider    string
	ExternalID  string // provider id of the sample; part of the dedup key
	Value       *float64
	ValueStruct json.RawMessage // structured reading; the daily merge reads Value only
	Unit        string
}

// MergedSample is the winning reading for one (data type, day).
type MergedSample struct {
	DataType capability.DataType
	Day      time.Time // UTC midnight
	Value    float64
	Unit     string
	Provider string // which provider won
}

// MergeDaily resolves, per (data type, UTC day), the winning sample by provider
// cascade (AnyProvider = wildcard); within the winner, the latest sample wins.
// Deterministic.
func MergeDaily(samples []Sample, priority []string) []MergedSample {
	if len(priority) == 0 {
		priority = []string{domain.AnyProvider}
	}
	rank := healthProviderRanker(priority)

	type key struct {
		dt  capability.DataType
		day int64
	}
	type best struct {
		rank int
		ts   time.Time
		s    Sample
	}
	winners := map[key]best{}
	for _, s := range samples {
		if s.Value == nil {
			continue
		}
		day := s.Timestamp.UTC().Truncate(24 * time.Hour)
		k := key{s.DataType, day.Unix()}
		r := rank(s.Provider)
		cur, ok := winners[k]
		if !ok || r < cur.rank || (r == cur.rank && s.Timestamp.After(cur.ts)) {
			winners[k] = best{rank: r, ts: s.Timestamp, s: s}
		}
	}

	out := make([]MergedSample, 0, len(winners))
	for k, b := range winners {
		out = append(out, MergedSample{
			DataType: b.s.DataType,
			Day:      time.Unix(k.day, 0).UTC(),
			Value:    *b.s.Value,
			Unit:     b.s.Unit,
			Provider: b.s.Provider,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DataType != out[j].DataType {
			return out[i].DataType < out[j].DataType
		}
		return out[i].Day.Before(out[j].Day)
	})
	return out
}

// healthProviderRanker returns a provider's cascade position (lower wins).
func healthProviderRanker(priority []string) func(string) int {
	named := map[string]struct{}{}
	for _, p := range priority {
		if p != domain.AnyProvider {
			named[p] = struct{}{}
		}
	}
	return func(provider string) int {
		for i, p := range priority {
			if p == provider {
				return i
			}
			if p == domain.AnyProvider {
				if _, isNamed := named[provider]; !isNamed {
					return i
				}
			}
		}
		return len(priority) + 1 // unranked → worst
	}
}
