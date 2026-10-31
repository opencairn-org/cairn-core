package fileimport

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cairnv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Manual builds the payload of a manually entered activity; an error names
// the first invalid field.
func Manual(in port.ManualActivity) (*cairnv1.ActivitySourcePayload, error) {
	typ, ok := protoActivityType(in.Type)
	if !ok {
		return nil, fmt.Errorf("invalid or missing type %q", in.Type)
	}
	start, err := time.Parse(time.RFC3339, in.StartTime)
	if err != nil {
		return nil, fmt.Errorf("invalid start_time (want RFC3339): %w", err)
	}
	if in.ElapsedSeconds <= 0 {
		return nil, fmt.Errorf("elapsed_duration_s must be > 0")
	}
	moving := in.MovingSeconds
	if moving <= 0 || moving > in.ElapsedSeconds {
		moving = in.ElapsedSeconds
	}
	tz := in.Timezone
	if tz == "" {
		tz = "UTC"
	}

	disc := cairnv1.Discipline_DISCIPLINE_UNSPECIFIED
	if in.Discipline != "" {
		d, ok := protoDiscipline(in.Discipline)
		if !ok {
			return nil, fmt.Errorf("invalid discipline %q", in.Discipline)
		}
		disc = d
	}

	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "Manual Activity"
	}

	elapsed := time.Duration(in.ElapsedSeconds * float64(time.Second))
	summary := &cairnv1.ActivitySummary{
		DistanceM:       in.DistanceM,
		ElevationGainM:  in.ElevationGainM,
		AvgHeartRateBpm: in.AvgHeartRateBpm,
		AvgPowerW:       in.AvgPowerW,
		CaloriesKcal:    in.CaloriesKcal,
	}

	return &cairnv1.ActivitySourcePayload{
		Type:            typ,
		Discipline:      disc,
		IsVirtual:       in.IsVirtual,
		IsEbike:         in.IsEbike,
		IsCommute:       in.IsCommute,
		IsRace:          in.IsRace,
		CustomSubtype:   in.CustomSubtype,
		Title:           title,
		Description:     in.Description,
		StartTime:       timestamppb.New(start.UTC()),
		EndTime:         timestamppb.New(start.Add(elapsed).UTC()),
		ElapsedDuration: durationpb.New(elapsed),
		MovingDuration:  durationpb.New(time.Duration(moving * float64(time.Second))),
		Timezone:        tz,
		Summary:         summary,
	}, nil
}

// protoActivityType maps a lowercase domain type ("ride") to its proto enum,
// reporting ok=false for empty/unknown/UNSPECIFIED.
func protoActivityType(s string) (cairnv1.ActivityType, bool) {
	if s == "" {
		return 0, false
	}
	v, ok := cairnv1.ActivityType_value["ACTIVITY_TYPE_"+strings.ToUpper(s)]
	if !ok || v == 0 {
		return 0, false
	}
	return cairnv1.ActivityType(v), true
}

// protoDiscipline maps a lowercase discipline ("ride_road") to its proto enum.
func protoDiscipline(s string) (cairnv1.Discipline, bool) {
	v, ok := cairnv1.Discipline_value["DISCIPLINE_"+strings.ToUpper(s)]
	if !ok || v == 0 {
		return 0, false
	}
	return cairnv1.Discipline(v), true
}
