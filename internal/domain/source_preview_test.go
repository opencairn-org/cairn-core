package domain

import (
	"testing"
	"time"
)

func TestDiffSourcePayloads(t *testing.T) {
	a, b := 10.0, 12.5
	hr := 140
	before := ActivitySourcePayload{Type: ActivityTypeRide, Title: "x", StartTime: time.Unix(0, 0), ElapsedDuration: time.Hour, Summary: ActivitySummary{DistanceM: &a}}
	after := before
	after.Summary.DistanceM = &b
	after.Summary.AvgHeartRateBpm = &hr
	after.IsRace = true
	got := DiffSourcePayloads(before, after)
	want := map[string][2]string{"distance_m": {"10.0", "12.5"}, "avg_heart_rate_bpm": {"", "140"}, "is_race": {"false", "true"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, c := range got {
		w, ok := want[c.Field]
		if !ok || w[0] != c.Before || w[1] != c.After {
			t.Fatalf("change %+v", c)
		}
	}
	if len(DiffActivities(Activity{}, Activity{})) != 0 {
		t.Fatal("identical activities must not differ")
	}
}
