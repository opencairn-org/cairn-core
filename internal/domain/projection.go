package domain

import "time"

// ProjectedActivity is an activity as a non-owner may see it: the summary tier
// is always present, every richer field only when its visibility category is
// granted. The REST projection and the Connect ProjectedActivity message are
// both rendered from it, so the two surfaces cannot drift.
type ProjectedActivity struct {
	ID              ActivityID
	Title           string
	Type            ActivityType
	Discipline      Discipline
	StartTime       time.Time
	Timezone        string
	ElapsedDuration time.Duration
	MovingDuration  time.Duration
	DistanceM       *float64
	ElevationGainM  *float64

	// CategorySummary
	Description  string
	TSS          *float64
	CaloriesKcal *int

	// CategoryLocation
	StartPlace            string
	StartLat, StartLng    *float64
	StartLocationRedacted bool // start inside one of the owner's privacy zones

	// CategoryHR / CategoryPower / CategoryPaceSpeed
	AvgHeartRateBpm, MaxHeartRateBpm       *int
	AvgPowerW, MaxPowerW, NormalizedPowerW *int
	AvgSpeedMps, MaxSpeedMps               *float64
	MapVisible, PhotosVisible              bool
	Visible                                []Category // AllCategories order
}

// ProjectActivity limits a to the granted categories. zones are the owner's
// privacy zones (nil for owner reads): a start point inside any zone hides the
// coordinates and the place name even when CategoryLocation is granted, and
// StartLocationRedacted says so.
func ProjectActivity(a Activity, cats CategorySet, zones []PrivacyZone) ProjectedActivity {
	p := ProjectedActivity{
		ID: a.ID, Title: a.Title, Type: a.Type, Discipline: a.Discipline,
		StartTime: a.StartTime.UTC(), Timezone: a.Timezone,
		ElapsedDuration: a.ElapsedDuration, MovingDuration: a.MovingDuration,
		DistanceM: a.Summary.DistanceM, ElevationGainM: a.Summary.ElevationGainM,
	}
	if cats.Has(CategorySummary) {
		p.Description = a.Description
		p.TSS = a.Summary.TSS
		p.CaloriesKcal = a.Summary.CaloriesKcal
	}
	if cats.Has(CategoryLocation) {
		if a.StartLat != nil && a.StartLng != nil && PointInAnyZone(zones, *a.StartLat, *a.StartLng) {
			p.StartLocationRedacted = true
		} else {
			p.StartPlace = a.StartPlace
			p.StartLat, p.StartLng = a.StartLat, a.StartLng
		}
	}
	if cats.Has(CategoryHR) {
		p.AvgHeartRateBpm, p.MaxHeartRateBpm = a.Summary.AvgHeartRateBpm, a.Summary.MaxHeartRateBpm
	}
	if cats.Has(CategoryPower) {
		p.AvgPowerW, p.MaxPowerW, p.NormalizedPowerW = a.Summary.AvgPowerW, a.Summary.MaxPowerW, a.Summary.NormalizedPowerW
	}
	if cats.Has(CategoryPaceSpeed) {
		p.AvgSpeedMps, p.MaxSpeedMps = a.Summary.AvgSpeedMps, a.Summary.MaxSpeedMps
	}
	p.MapVisible = cats.Has(CategoryMap)
	p.PhotosVisible = cats.Has(CategoryPhotos)
	for _, c := range AllCategories {
		if cats.Has(c) {
			p.Visible = append(p.Visible, c)
		}
	}
	return p
}
