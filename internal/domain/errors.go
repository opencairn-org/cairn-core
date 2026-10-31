package domain

import "errors"

// ---------------------------------------------------------------------------
// Sentinel errors for domain-level invariants.
//
// Use cases and adapters wrap these with %w so callers can branch with
// errors.Is and translate them to Connect/HTTP error codes.
// ---------------------------------------------------------------------------

// Activity invariants — checked by Activity.Validate.
var (
	ErrActivityMissingID              = errors.New("activity: id is required")
	ErrActivityMissingUserID          = errors.New("activity: user_id is required")
	ErrActivityInvalidType            = errors.New("activity: type is invalid")
	ErrActivityDisciplineIncompatible = errors.New("activity: discipline does not match type")
	ErrActivityInvalidPrivacy         = errors.New("activity: privacy is invalid")
	ErrActivityTimeOrder              = errors.New("activity: end_time precedes start_time")
	ErrActivityNegativeDuration       = errors.New("activity: duration is negative")
	ErrActivityMovingExceedsElapsed   = errors.New("activity: moving_duration exceeds elapsed_duration")
	ErrActivityMissingTimezone        = errors.New("activity: timezone is required")
	ErrInvalidAthleteMetric           = errors.New("athlete metric: invalid")
)

// Merge engine errors.
var (
	// ErrNoSources is returned by the merge engine when called with an
	// empty source set. The caller (dedup pipeline) should never reach
	// this — it is a programmer error.
	ErrNoSources = errors.New("merge: no sources to merge")

	// ErrSourcesMismatchedUser is returned when sources from different
	// users are passed together. Indicates a serious bug; merge refuses.
	ErrSourcesMismatchedUser = errors.New("merge: sources belong to different users")

	// ErrNoClassification is returned when no source provides a usable
	// ActivityType. The merge cannot produce a valid Activity in this case
	// and the import should be rejected with a worker bug report.
	ErrNoClassification = errors.New("merge: no source provided a valid activity type")
)

// Dedup pipeline errors.
var (
	// ErrAmbiguousDedupMatch is returned when stage 2 (heuristic) finds
	// multiple candidate activities. The caller logs a warning and proceeds
	// with the closest-in-time match.
	ErrAmbiguousDedupMatch = errors.New("dedup: multiple candidate activities matched")
)

// Generic lookup errors. Repositories implement these so use cases can
// branch on "not found" without each implementation defining its own.
var (
	ErrNotFound = errors.New("not found")
	// ErrAmbiguous: a lookup that must resolve exactly one row matched several.
	ErrAmbiguous = errors.New("ambiguous match")
	// ErrUnique is wrapped by repo writes that violate a uniqueness
	// constraint. Use cases that race on insert/lookup branch on this.
	ErrUnique = errors.New("unique constraint violation")
)

// Gear invariants — checked by Gear.Validate.
var (
	ErrGearMissingID     = errors.New("gear: missing ID")
	ErrGearMissingUserID = errors.New("gear: missing user ID")
	ErrGearInvalidKind   = errors.New("gear: kind must be bike, shoe or other")
	ErrGearMissingName   = errors.New("gear: name is required")
	// ErrGearFieldTooLong is returned when a free-text field exceeds its cap.
	ErrGearFieldTooLong = errors.New("gear: field too long")
	// ErrGearSelfMerge is returned by MergeGear when from and to are the same record.
	ErrGearSelfMerge = errors.New("gear: cannot merge a record into itself")
	// ErrGearRefMapped is returned when a decision is made on a provider record that already maps to gear.
	ErrGearRefMapped = errors.New("gear: provider record is already mapped")
)

// ErrParserDowngrade rejects a result from an older parser than the one that
// last parsed the source (docs/adr/0005: versions only move forward).
var ErrParserDowngrade = errors.New("source: parser version older than the stored one")

// Segment invariants — checked by Segment.Validate.
var (
	ErrSegmentInvalidSource            = errors.New("segment: source must be external or native")
	ErrSegmentInvalidScope             = errors.New("segment: scope must be private or instance")
	ErrSegmentInvalidActivityType      = errors.New("segment: activity_type is invalid")
	ErrSegmentExternalMissingExternal  = errors.New("segment: external source requires external_id and external_account_id")
	ErrSegmentExternalUnexpectedOwner  = errors.New("segment: external source must not set owner_user_id")
	ErrSegmentExternalScopeNotPrivate  = errors.New("segment: external-mirror segments must be private")
	ErrSegmentNativeMissingOwner       = errors.New("segment: native source requires owner_user_id")
	ErrSegmentNativeUnexpectedExternal = errors.New("segment: native source must not set external_id / external_account_id")
	ErrSegmentMissingPolyline          = errors.New("segment: polyline is required")
	ErrSegmentInvalidPolylinePrecision = errors.New("segment: polyline_precision must be 5 or 6")
	ErrSegmentNonPositiveDistance      = errors.New("segment: distance_m must be positive")
)

// IsActivityValidationError reports whether err is a terminal activity-validation
// failure (bad data) — retrying the same payload won't help, so the ingest queue
// should fail the item rather than redeliver forever.
func IsActivityValidationError(err error) bool {
	for _, e := range []error{
		ErrActivityMissingID, ErrActivityMissingUserID, ErrActivityInvalidType,
		ErrActivityDisciplineIncompatible, ErrActivityInvalidPrivacy, ErrActivityTimeOrder,
		ErrActivityNegativeDuration, ErrActivityMovingExceedsElapsed, ErrActivityMissingTimezone,
		ErrNoClassification, ErrParserDowngrade,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
