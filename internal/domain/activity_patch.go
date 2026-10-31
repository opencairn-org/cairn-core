package domain

import "time"

// Clearable is an edit to a nullable overlay field; a nil Value reverts it to
// the merged value.
type Clearable[T any] struct{ Value *T }

// GearEdit is a user gear choice: Reset hands it back to the merge, otherwise
// GearID is attached (nil = explicitly no gear).
type GearEdit struct {
	GearID *GearID
	Reset  bool
}

// ActivityPatch is an owner edit of one activity; a nil field is untouched.
type ActivityPatch struct {
	Title       *string
	Description *string
	// Tags replaces the whole set (empty clears it).
	Tags    *[]string
	Privacy *ActivityPrivacy
	Gear    *GearEdit
	// PrimaryStreamSourceID is merge-managed (gps_track pin) and always rejected.
	PrimaryStreamSourceID *SourceID
	Overlay               OverlayPatch
}

// Empty reports whether the patch touches no field.
func (p ActivityPatch) Empty() bool {
	return p.Title == nil && p.Description == nil && p.Tags == nil && p.Privacy == nil &&
		p.Gear == nil && p.PrimaryStreamSourceID == nil && p.Overlay.Empty()
}

// OverlayPatch edits the ClassificationOverride overlay; a nil field is untouched.
type OverlayPatch struct {
	Type           *Clearable[ActivityType]
	Discipline     *Clearable[Discipline]
	IsVirtual      *bool
	IsCommute      *bool
	IsRace         *bool
	IsEbike        *bool
	CustomSubtype  *Clearable[string]
	DistanceM      *Clearable[float64]
	ElevationGainM *Clearable[float64]
	MovingDuration *Clearable[time.Duration]
}

// Empty reports whether the overlay patch touches no field.
func (p OverlayPatch) Empty() bool {
	return p.FirstField() == ""
}

// FirstField names the first touched overlay field ("" when none), for errors.
func (p OverlayPatch) FirstField() string {
	switch {
	case p.Type != nil:
		return "type"
	case p.Discipline != nil:
		return "discipline"
	case p.IsVirtual != nil:
		return "is_virtual"
	case p.IsCommute != nil:
		return "is_commute"
	case p.IsRace != nil:
		return "is_race"
	case p.IsEbike != nil:
		return "is_ebike"
	case p.CustomSubtype != nil:
		return "custom_subtype"
	case p.DistanceM != nil:
		return "distance_m"
	case p.ElevationGainM != nil:
		return "elevation_gain_m"
	case p.MovingDuration != nil:
		return "moving_duration_s"
	}
	return ""
}

// ApplyTo writes the touched fields onto co.
func (p OverlayPatch) ApplyTo(co *ClassificationOverride) {
	applyClearable(&co.Type, p.Type)
	applyClearable(&co.Discipline, p.Discipline)
	applyBool(&co.IsVirtual, p.IsVirtual)
	applyBool(&co.IsCommute, p.IsCommute)
	applyBool(&co.IsRace, p.IsRace)
	applyBool(&co.IsEbike, p.IsEbike)
	applyClearable(&co.CustomSubtype, p.CustomSubtype)
	applyClearable(&co.DistanceM, p.DistanceM)
	applyClearable(&co.ElevationGainM, p.ElevationGainM)
	applyClearable(&co.MovingDuration, p.MovingDuration)
}

func applyClearable[T any](dst **T, c *Clearable[T]) {
	if c == nil {
		return
	}
	if c.Value == nil {
		*dst = nil
		return
	}
	v := *c.Value
	*dst = &v
}

func applyBool(dst **bool, v *bool) {
	if v == nil {
		return
	}
	b := *v
	*dst = &b
}
