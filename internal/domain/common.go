// Package domain defines Cairn's core types and pure business logic.
//
// The domain layer has no I/O dependencies (no DB, no HTTP, no protobuf
// imports). Adapters in internal/adapter translate between the wire types
// (Connect-RPC, protobuf, sqlc rows) and these domain types.
//
// Files:
//
//	common.go     - IDs, shared enums (ActivityType, Discipline, …), helpers
//	activity.go   - Activity aggregate root and its summary fields
//	source.go     - ActivitySource and ActivitySourcePayload from workers
//	merge.go      - FieldGroup, MergePolicy, MergeProvenance
//	errors.go     - sentinel errors
package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Typed UUIDs
//
// Each domain ID is a distinct type to prevent accidental cross-wiring
// (you can't pass a UserID where an ActivityID is required).
// ---------------------------------------------------------------------------

type (
	UserID                  uuid.UUID
	ActivityID              uuid.UUID
	SourceID                uuid.UUID
	ExternalAccountID       uuid.UUID
	GearID                  uuid.UUID
	SegmentID               uuid.UUID
	SegmentEffortID         uuid.UUID
	BestEffortID            uuid.UUID
	CustomBestEffortID      uuid.UUID
	MetricID                uuid.UUID
	NotificationID          uuid.UUID
	WorkerEnrollmentID      uuid.UUID
	WorkerCredentialGrantID uuid.UUID
	SigningKeyID            uuid.UUID
	SessionID               uuid.UUID
	LinkedIdentityID        uuid.UUID
	AthleteMetricID         uuid.UUID
	PasskeyID               uuid.UUID
	InviteID                uuid.UUID
	PATID                   uuid.UUID
	WebhookEndpointID       uuid.UUID
	PrivacyZoneID           uuid.UUID
	CommentID               uuid.UUID
	ContentReportID         uuid.UUID
	MatchConstraintID       uuid.UUID
	AttachmentID            uuid.UUID
)

// UUID returns the underlying uuid.UUID. Each ID type has the same method
// so generic code can pull the raw value.
func (id UserID) UUID() uuid.UUID                  { return uuid.UUID(id) }
func (id ActivityID) UUID() uuid.UUID              { return uuid.UUID(id) }
func (id SourceID) UUID() uuid.UUID                { return uuid.UUID(id) }
func (id ExternalAccountID) UUID() uuid.UUID       { return uuid.UUID(id) }
func (id GearID) UUID() uuid.UUID                  { return uuid.UUID(id) }
func (id SegmentID) UUID() uuid.UUID               { return uuid.UUID(id) }
func (id SegmentEffortID) UUID() uuid.UUID         { return uuid.UUID(id) }
func (id BestEffortID) UUID() uuid.UUID            { return uuid.UUID(id) }
func (id CustomBestEffortID) UUID() uuid.UUID      { return uuid.UUID(id) }
func (id MetricID) UUID() uuid.UUID                { return uuid.UUID(id) }
func (id NotificationID) UUID() uuid.UUID          { return uuid.UUID(id) }
func (id WorkerEnrollmentID) UUID() uuid.UUID      { return uuid.UUID(id) }
func (id WorkerCredentialGrantID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id SigningKeyID) UUID() uuid.UUID            { return uuid.UUID(id) }
func (id SessionID) UUID() uuid.UUID               { return uuid.UUID(id) }
func (id LinkedIdentityID) UUID() uuid.UUID        { return uuid.UUID(id) }
func (id AthleteMetricID) UUID() uuid.UUID         { return uuid.UUID(id) }
func (id PasskeyID) UUID() uuid.UUID               { return uuid.UUID(id) }
func (id InviteID) UUID() uuid.UUID                { return uuid.UUID(id) }
func (id PATID) UUID() uuid.UUID                   { return uuid.UUID(id) }
func (id WebhookEndpointID) UUID() uuid.UUID       { return uuid.UUID(id) }
func (id PrivacyZoneID) UUID() uuid.UUID           { return uuid.UUID(id) }
func (id CommentID) UUID() uuid.UUID               { return uuid.UUID(id) }
func (id ContentReportID) UUID() uuid.UUID         { return uuid.UUID(id) }
func (id MatchConstraintID) UUID() uuid.UUID       { return uuid.UUID(id) }
func (id AttachmentID) UUID() uuid.UUID            { return uuid.UUID(id) }

// String makes IDs printable as their canonical UUID string.
func (id UserID) String() string                  { return uuid.UUID(id).String() }
func (id ActivityID) String() string              { return uuid.UUID(id).String() }
func (id SourceID) String() string                { return uuid.UUID(id).String() }
func (id ExternalAccountID) String() string       { return uuid.UUID(id).String() }
func (id GearID) String() string                  { return uuid.UUID(id).String() }
func (id SegmentID) String() string               { return uuid.UUID(id).String() }
func (id SegmentEffortID) String() string         { return uuid.UUID(id).String() }
func (id BestEffortID) String() string            { return uuid.UUID(id).String() }
func (id CustomBestEffortID) String() string      { return uuid.UUID(id).String() }
func (id MetricID) String() string                { return uuid.UUID(id).String() }
func (id NotificationID) String() string          { return uuid.UUID(id).String() }
func (id WorkerEnrollmentID) String() string      { return uuid.UUID(id).String() }
func (id WorkerCredentialGrantID) String() string { return uuid.UUID(id).String() }
func (id SigningKeyID) String() string            { return uuid.UUID(id).String() }
func (id SessionID) String() string               { return uuid.UUID(id).String() }
func (id LinkedIdentityID) String() string        { return uuid.UUID(id).String() }
func (id AthleteMetricID) String() string         { return uuid.UUID(id).String() }
func (id PasskeyID) String() string               { return uuid.UUID(id).String() }
func (id InviteID) String() string                { return uuid.UUID(id).String() }
func (id PATID) String() string                   { return uuid.UUID(id).String() }
func (id WebhookEndpointID) String() string       { return uuid.UUID(id).String() }
func (id PrivacyZoneID) String() string           { return uuid.UUID(id).String() }
func (id CommentID) String() string               { return uuid.UUID(id).String() }
func (id ContentReportID) String() string         { return uuid.UUID(id).String() }
func (id MatchConstraintID) String() string       { return uuid.UUID(id).String() }
func (id AttachmentID) String() string            { return uuid.UUID(id).String() }

// ---------------------------------------------------------------------------
// ActivityType — top-level discipline categorisation
// ---------------------------------------------------------------------------

// ActivityType is the canonical top-level kind of workout. Backed by a
// CHECK constraint in DB migration 00005 (column activities.type) and
// by the cairn.v1.ActivityType proto enum.
type ActivityType string

const (
	ActivityTypeRide    ActivityType = "ride"
	ActivityTypeRun     ActivityType = "run"
	ActivityTypeSwim    ActivityType = "swim"
	ActivityTypeHike    ActivityType = "hike"
	ActivityTypeWalk    ActivityType = "walk"
	ActivityTypeRow     ActivityType = "row"
	ActivityTypeSki     ActivityType = "ski"
	ActivityTypeWorkout ActivityType = "workout" // catch-all (strength, yoga, etc.)

	// Extended sport set. Coarse top-level kinds only — finer splits live in
	// Discipline. Keep in sync with the cairn.v1.ActivityType proto enum and
	// the activities/segments/best_efforts type CHECK constraints (migration
	// 00057).
	ActivityTypeSnowboard  ActivityType = "snowboard"
	ActivityTypeSkate      ActivityType = "skate" // inline + ice skating
	ActivityTypeKayak      ActivityType = "kayak" // kayak + canoe
	ActivityTypeSUP        ActivityType = "sup"
	ActivityTypeSurf       ActivityType = "surf" // surf + windsurf + kitesurf
	ActivityTypeGolf       ActivityType = "golf"
	ActivityTypeClimb      ActivityType = "climb" // rock / indoor / bouldering
	ActivityTypeTennis     ActivityType = "tennis"
	ActivityTypeElliptical ActivityType = "elliptical"
	ActivityTypeWheelchair ActivityType = "wheelchair" // wheelchair + handcycle
)

// AllActivityTypes is the iteration order used by UI listings and bulk ops.
var AllActivityTypes = []ActivityType{
	ActivityTypeRide, ActivityTypeRun, ActivityTypeSwim, ActivityTypeHike,
	ActivityTypeWalk, ActivityTypeRow, ActivityTypeSki, ActivityTypeWorkout,
	ActivityTypeSnowboard, ActivityTypeSkate, ActivityTypeKayak, ActivityTypeSUP,
	ActivityTypeSurf, ActivityTypeGolf, ActivityTypeClimb, ActivityTypeTennis,
	ActivityTypeElliptical, ActivityTypeWheelchair,
}

// Valid reports whether t is one of the known activity types.
func (t ActivityType) Valid() bool {
	switch t {
	case ActivityTypeRide, ActivityTypeRun, ActivityTypeSwim, ActivityTypeHike,
		ActivityTypeWalk, ActivityTypeRow, ActivityTypeSki, ActivityTypeWorkout,
		ActivityTypeSnowboard, ActivityTypeSkate, ActivityTypeKayak, ActivityTypeSUP,
		ActivityTypeSurf, ActivityTypeGolf, ActivityTypeClimb, ActivityTypeTennis,
		ActivityTypeElliptical, ActivityTypeWheelchair:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Discipline — orthogonal sub-classification
//
// Disciplines are pinned to a parent ActivityType. RIDE_ROAD is only valid
// when ActivityType=RIDE. The check is enforced by Discipline.CompatibleWith.
// ---------------------------------------------------------------------------

type Discipline string

const (
	DisciplineNone Discipline = ""

	DisciplineRideRoad       Discipline = "ride_road"
	DisciplineRideMTB        Discipline = "ride_mtb"
	DisciplineRideGravel     Discipline = "ride_gravel"
	DisciplineRideCyclocross Discipline = "ride_cyclocross"
	DisciplineRideTrack      Discipline = "ride_track"
	DisciplineRideBMX        Discipline = "ride_bmx"

	DisciplineRunRoad  Discipline = "run_road"
	DisciplineRunTrail Discipline = "run_trail"
	DisciplineRunTrack Discipline = "run_track"

	DisciplineSwimPool      Discipline = "swim_pool"
	DisciplineSwimOpenWater Discipline = "swim_open_water"

	DisciplineSkiAlpine      Discipline = "ski_alpine"
	DisciplineSkiNordic      Discipline = "ski_nordic"
	DisciplineSkiTouring     Discipline = "ski_touring"
	DisciplineSkiBackcountry Discipline = "ski_backcountry"
)

// CompatibleWith reports whether d is a valid discipline for activity type t.
// DisciplineNone is compatible with every type.
func (d Discipline) CompatibleWith(t ActivityType) bool {
	if d == DisciplineNone {
		return true
	}
	switch t {
	case ActivityTypeRide:
		switch d {
		case DisciplineRideRoad, DisciplineRideMTB, DisciplineRideGravel,
			DisciplineRideCyclocross, DisciplineRideTrack, DisciplineRideBMX:
			return true
		}
	case ActivityTypeRun:
		switch d {
		case DisciplineRunRoad, DisciplineRunTrail, DisciplineRunTrack:
			return true
		}
	case ActivityTypeSwim:
		switch d {
		case DisciplineSwimPool, DisciplineSwimOpenWater:
			return true
		}
	case ActivityTypeSki:
		switch d {
		case DisciplineSkiAlpine, DisciplineSkiNordic, DisciplineSkiTouring, DisciplineSkiBackcountry:
			return true
		}
	}
	return false
}

// ParentType returns the ActivityType to which d belongs. Returns ("", false)
// for DisciplineNone or unknown values.
func (d Discipline) ParentType() (ActivityType, bool) {
	switch d {
	case DisciplineRideRoad, DisciplineRideMTB, DisciplineRideGravel,
		DisciplineRideCyclocross, DisciplineRideTrack, DisciplineRideBMX:
		return ActivityTypeRide, true
	case DisciplineRunRoad, DisciplineRunTrail, DisciplineRunTrack:
		return ActivityTypeRun, true
	case DisciplineSwimPool, DisciplineSwimOpenWater:
		return ActivityTypeSwim, true
	case DisciplineSkiAlpine, DisciplineSkiNordic, DisciplineSkiTouring, DisciplineSkiBackcountry:
		return ActivityTypeSki, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// ActivityPrivacy
// ---------------------------------------------------------------------------

type ActivityPrivacy string

const (
	PrivacyPrivate   ActivityPrivacy = "private"
	PrivacyFollowers ActivityPrivacy = "followers"
	PrivacyPublic    ActivityPrivacy = "public"
)

func (p ActivityPrivacy) Valid() bool {
	switch p {
	case PrivacyPrivate, PrivacyFollowers, PrivacyPublic:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Pointer helpers
//
// Domain types use *T for nullable summary fields ("the source did not
// provide this value" vs "the value is zero"). These helpers reduce
// boilerplate at call sites without pulling in a generics package.
// ---------------------------------------------------------------------------

// Ptr returns a pointer to v. Useful for constructing test fixtures and
// when assigning literals to *T fields.
func Ptr[T any](v T) *T { return &v }

// Deref returns *p if non-nil, else zero.
func Deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// ParseUUID is a small helper that converts a string into the requested
// typed-UUID. Returns the typed zero value and an error on parse failure.
//
// Example:
//
//	id, err := domain.ParseUUID[domain.ActivityID](raw)
func ParseUUID[T ~[16]byte](s string) (T, error) {
	var zero T
	u, err := uuid.Parse(s)
	if err != nil {
		return zero, fmt.Errorf("parse uuid: %w", err)
	}
	return T(u), nil
}
