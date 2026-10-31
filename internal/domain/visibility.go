package domain

import (
	"math"
	"time"
)

// Per-field visibility model (multi-user v1). See docs/visibility-model.md.
//
// A viewer's access to another user's activity is resolved PER CATEGORY, not
// all-or-nothing: resolve the viewer's Audience (owner > link > followers >
// public), then look up which Categories that audience may see under the
// owner's VisibilityPolicy. Every non-owner read path projects an activity
// through the resulting CategorySet so nothing leaks.

// Audience is the viewer's relationship to an activity's owner, most-privileged
// first. Exactly one applies to a given (viewer, activity).
type Audience string

const (
	AudienceOwner     Audience = "owner"     // the owner — always sees everything
	AudienceLink      Audience = "link"      // holder of a valid share token
	AudienceFollowers Audience = "followers" // an accepted follower
	AudiencePublic    Audience = "public"    // anyone
)

// Category is an independently-gateable slice of an activity's data.
type Category string

const (
	CategorySummary     Category = "summary"      // type, title, date, distance, time, elevation, calories
	CategoryMap         Category = "map"          // GPS track (privacy-zone masked for non-owners)
	CategoryPhotos      Category = "photos"       // activity photos / attachments
	CategoryLocation    Category = "location"     // start place + start/end coords
	CategoryHR          Category = "hr"           // heart-rate stream + avg/max
	CategoryPower       Category = "power"        // power stream + avg/max/NP
	CategoryCadence     Category = "cadence"      // cadence stream
	CategoryPaceSpeed   Category = "pace_speed"   // speed/pace stream + avg/max
	CategorySegments    Category = "segments"     // segment efforts + placement
	CategoryBestEfforts Category = "best_efforts" // best-effort curves
	CategoryZones       Category = "zones"        // time-in-zone
	CategorySocial      Category = "social"       // may kudos/comment (a gate, not data)
)

// AllCategories is the canonical iteration order.
var AllCategories = []Category{
	CategorySummary, CategoryMap, CategoryPhotos, CategoryLocation, CategoryHR, CategoryPower,
	CategoryCadence, CategoryPaceSpeed, CategorySegments, CategoryBestEfforts,
	CategoryZones, CategorySocial,
}

// CategorySet is a set of allowed categories.
type CategorySet map[Category]bool

func categorySetOf(cats ...Category) CategorySet {
	s := make(CategorySet, len(cats))
	for _, c := range cats {
		s[c] = true
	}
	return s
}

// Has reports whether the category is allowed.
func (s CategorySet) Has(c Category) bool { return s[c] }

// Slice returns the allowed categories in canonical order (for serialisation).
func (s CategorySet) Slice() []Category {
	out := make([]Category, 0, len(s))
	for _, c := range AllCategories {
		if s[c] {
			out = append(out, c)
		}
	}
	return out
}

// VisibilityPolicy maps each non-owner audience to the categories it may see.
// The owner audience is implicit (everything) and never stored.
type VisibilityPolicy struct {
	ByAudience map[Audience]CategorySet
}

// DefaultVisibilityPolicy is the privacy-leaning fallback when a user hasn't
// customised their visibility settings (see docs/visibility-model.md):
//   - public:    summary only
//   - followers: the workout, minus HR/power unless opted in
//   - link:      same as followers (an explicit share is a trust signal)
func DefaultVisibilityPolicy() VisibilityPolicy {
	followers := categorySetOf(
		CategorySummary, CategoryMap, CategoryPhotos, CategoryLocation, CategoryPaceSpeed,
		CategorySegments, CategoryBestEfforts, CategoryZones, CategorySocial,
	)
	return VisibilityPolicy{ByAudience: map[Audience]CategorySet{
		AudiencePublic:    categorySetOf(CategorySummary),
		AudienceFollowers: followers,
		AudienceLink:      followers,
	}}
}

// allowedFor returns the category set this policy grants the audience, falling
// back to the default policy for any audience the policy doesn't define.
func (p VisibilityPolicy) allowedFor(a Audience) CategorySet {
	if p.ByAudience != nil {
		if s, ok := p.ByAudience[a]; ok {
			return s
		}
	}
	return DefaultVisibilityPolicy().ByAudience[a]
}

// VisibilityInput is the relationship context for resolving what a viewer sees.
type VisibilityInput struct {
	IsOwner        bool // viewer == owner
	HasValidLink   bool // viewer presented a valid, unrevoked share token
	IsFollower     bool // viewer is an accepted follower of the owner
	OwnerPolicy    VisibilityPolicy
	ActivityPolicy *VisibilityPolicy // optional per-activity override (merged over owner default)

	Privacy ActivityPrivacy // the activity's privacy; unknown values count as private
	Hidden  bool            // hidden by an admin
	Deleted bool            // soft-deleted
}

// VisibilityInputFor seeds the activity-state fields from act.
func VisibilityInputFor(act *Activity) VisibilityInput {
	return VisibilityInput{Privacy: act.Privacy, Hidden: act.HiddenByAdmin, Deleted: act.IsDeleted()}
}

// ResolveAudience picks the single most-privileged audience for a viewer.
func ResolveAudience(in VisibilityInput) Audience {
	switch {
	case in.IsOwner:
		return AudienceOwner
	case in.HasValidLink:
		return AudienceLink
	case in.IsFollower:
		return AudienceFollowers
	default:
		return AudiencePublic
	}
}

// ResolveVisibility returns the categories a viewer may see for an activity.
// The owner always sees everything. Non-owners see nothing of a deleted or
// admin-hidden activity; a share link opens a private one; otherwise private
// shuts everyone out and followers-only shuts out non-followers. What is left
// comes from the per-activity override, else the owner's default policy.
func ResolveVisibility(in VisibilityInput) CategorySet {
	aud := ResolveAudience(in)
	if aud == AudienceOwner {
		return categorySetOf(AllCategories...)
	}
	if !privacyAdmits(in, aud) {
		return CategorySet{}
	}
	if in.ActivityPolicy != nil {
		if s, ok := in.ActivityPolicy.ByAudience[aud]; ok {
			return s
		}
	}
	return in.OwnerPolicy.allowedFor(aud)
}

// privacyAdmits applies the activity's state and privacy enum to a non-owner audience.
func privacyAdmits(in VisibilityInput, aud Audience) bool {
	if in.Deleted || in.Hidden {
		return false
	}
	if aud == AudienceLink {
		return true
	}
	switch in.Privacy {
	case PrivacyPublic:
		return true
	case PrivacyFollowers:
		return aud == AudienceFollowers
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Privacy zones
// ---------------------------------------------------------------------------

// PrivacyZone is a circular area (home, work) within which an owner's GPS is
// masked from non-owner viewers.
type PrivacyZone struct {
	ID        PrivacyZoneID
	UserID    UserID
	Label     string
	Lat       float64
	Lng       float64
	RadiusM   float64
	CreatedAt time.Time
}

// Contains reports whether (lat, lng) lies within the zone's radius, using the
// haversine great-circle distance.
func (z PrivacyZone) Contains(lat, lng float64) bool {
	return haversineMeters(z.Lat, z.Lng, lat, lng) <= z.RadiusM
}

// PointInAnyZone reports whether a point falls inside any of the zones.
func PointInAnyZone(zones []PrivacyZone, lat, lng float64) bool {
	for _, z := range zones {
		if z.Contains(lat, lng) {
			return true
		}
	}
	return false
}

func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const earthR = 6371000.0
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat := rad(lat2 - lat1)
	dLng := rad(lng2 - lng1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(rad(lat1))*math.Cos(rad(lat2))*math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthR * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// ---------------------------------------------------------------------------
// Policy JSON (jsonb storage shape: {audience: [category, ...]})
// ---------------------------------------------------------------------------

// MarshalPolicy converts a VisibilityPolicy to its jsonb shape.
func MarshalPolicy(p VisibilityPolicy) map[string][]string {
	out := map[string][]string{}
	for aud, set := range p.ByAudience {
		cats := set.Slice()
		strs := make([]string, len(cats))
		for i, c := range cats {
			strs[i] = string(c)
		}
		out[string(aud)] = strs
	}
	return out
}

// UnmarshalPolicy builds a VisibilityPolicy from its jsonb shape. An empty map
// yields an empty policy (callers fall back to DefaultVisibilityPolicy).
func UnmarshalPolicy(raw map[string][]string) VisibilityPolicy {
	if len(raw) == 0 {
		return VisibilityPolicy{}
	}
	by := map[Audience]CategorySet{}
	for aud, cats := range raw {
		set := make(CategorySet, len(cats))
		for _, c := range cats {
			set[Category(c)] = true
		}
		by[Audience(aud)] = set
	}
	return VisibilityPolicy{ByAudience: by}
}
