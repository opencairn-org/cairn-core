package domain

// CopySummaryGroups copies donor's summary values for the given groups into
// dst, leaving every other field nil, so a source carrying dst Provides only
// those groups.
func CopySummaryGroups(dst *ActivitySummary, donor ActivitySummary, groups []FieldGroup) {
	for _, g := range groups {
		switch g {
		case FieldGroupDistance:
			dst.DistanceM = donor.DistanceM
		case FieldGroupElevation:
			dst.ElevationGainM = donor.ElevationGainM
			dst.ElevationLossM = donor.ElevationLossM
			dst.MinElevationM = donor.MinElevationM
			dst.MaxElevationM = donor.MaxElevationM
		case FieldGroupSpeed:
			dst.AvgSpeedMps = donor.AvgSpeedMps
			dst.MaxSpeedMps = donor.MaxSpeedMps
		case FieldGroupHeartRate:
			dst.AvgHeartRateBpm = donor.AvgHeartRateBpm
			dst.MaxHeartRateBpm = donor.MaxHeartRateBpm
		case FieldGroupPower:
			dst.AvgPowerW = donor.AvgPowerW
			dst.MaxPowerW = donor.MaxPowerW
			dst.NormalizedPowerW = donor.NormalizedPowerW
		case FieldGroupCadence:
			dst.AvgCadence = donor.AvgCadence
			dst.MaxCadence = donor.MaxCadence
		case FieldGroupTemperature:
			dst.AvgTemperatureC = donor.AvgTemperatureC
			dst.MinTemperatureC = donor.MinTemperatureC
			dst.MaxTemperatureC = donor.MaxTemperatureC
		case FieldGroupCalories:
			dst.CaloriesKcal = donor.CaloriesKcal
		}
	}
}
