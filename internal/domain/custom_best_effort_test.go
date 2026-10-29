package domain

import "testing"

func TestCustomBestEffortValidate(t *testing.T) {
	ok := CustomBestEffort{ActivityType: ActivityTypeRun, Metric: BestEffortMetricPace, WindowKind: BestEffortWindowDistance, WindowValue: 7500, Label: " loop "}
	if err := ok.Validate(); err != nil || ok.Label != "loop" {
		t.Fatalf("valid: %v %q", err, ok.Label)
	}
	bad := map[string]CustomBestEffort{
		"no label":          {ActivityType: ActivityTypeRun, Metric: BestEffortMetricPace, WindowKind: BestEffortWindowDistance, WindowValue: 7500},
		"power by distance": {ActivityType: ActivityTypeRide, Metric: BestEffortMetricPower, WindowKind: BestEffortWindowDistance, WindowValue: 7500, Label: "x"},
		"pace by duration":  {ActivityType: ActivityTypeRun, Metric: BestEffortMetricPace, WindowKind: BestEffortWindowDuration, WindowValue: 600, Label: "x"},
		"too short":         {ActivityType: ActivityTypeRide, Metric: BestEffortMetricPower, WindowKind: BestEffortWindowDuration, WindowValue: 2, Label: "x"},
		"too far":           {ActivityType: ActivityTypeRun, Metric: BestEffortMetricPace, WindowKind: BestEffortWindowDistance, WindowValue: 900_000, Label: "x"},
		"bad type":          {ActivityType: "yoga", Metric: BestEffortMetricPower, WindowKind: BestEffortWindowDuration, WindowValue: 60, Label: "x"},
		"no kind":           {ActivityType: ActivityTypeRun, Metric: BestEffortMetricPower, WindowValue: 60, Label: "x"},
	}
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
