package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// BoundingBox is the rectangular spatial filter passed to
// ListSegmentCandidatesForActivity. WGS84 decimal degrees.
//
// The caller (matching use case) computes the bounding box from the
// activity's stream — min/max lat & lon across all samples — and adds a
// generous buffer so segments whose start lies just outside the strict
// bbox are still returned for evaluation.
type BoundingBox struct {
	MinLat float64
	MaxLat float64
	MinLon float64
	MaxLon float64
}

// SegmentRepo persists segments and their efforts.
//
// The spatial filter happens at the repo boundary so we can leverage the
// PostGIS GIST index on segments.geom. The pure matching algorithm in
// usecase/segment then walks the (small) candidate set in Go.
type SegmentRepo interface {
	// -------------------------------------------------------------------
	// Segment CRUD
	// -------------------------------------------------------------------

	// GetSegment returns one segment by ID. Returns domain.ErrNotFound
	// when no row exists.
	GetSegment(ctx context.Context, id domain.SegmentID) (domain.Segment, error)

	// SaveSegment upserts a segment by ID. Adapters are responsible for
	// reconstructing the PostGIS geom and bbox columns from the encoded
	// polyline; the caller passes only the domain shape.
	SaveSegment(ctx context.Context, s domain.Segment) error

	// FindSegmentIDByExternal resolves an external (provider-mirrored) segment
	// by (account, external_id) for dedup during ingest. Returns
	// domain.ErrNotFound when none exists yet.
	FindSegmentIDByExternal(ctx context.Context, accountID domain.ExternalAccountID, externalID string) (domain.SegmentID, error)

	// DeleteSegment hard-deletes a segment AND cascades to its efforts
	// via FK ON DELETE CASCADE. Use cases must restrict who can call:
	// Cairn-native segments by the owner, Strava-mirrored segments only
	// by the Strava worker reconciliation pass.
	DeleteSegment(ctx context.Context, id domain.SegmentID) error

	// -------------------------------------------------------------------
	// Spatial candidate filter (drives matching)
	// -------------------------------------------------------------------

	// ListSegmentCandidatesForActivity returns segments visible to the
	// user whose bbox intersects the supplied box AND whose activity_type
	// matches. Visibility rules:
	//
	//   * Strava-mirrored: only segments owned by one of this user's
	//     external accounts.
	//   * Cairn-native PRIVATE: only segments where owner_user_id == userID.
	//   * Cairn-native INSTANCE: all instance-scope segments.
	//
	// Adapters apply these rules in SQL; the caller does not need to
	// re-filter.
	ListSegmentCandidatesForActivity(
		ctx context.Context,
		userID domain.UserID,
		activityType domain.ActivityType,
		box BoundingBox,
	) ([]domain.Segment, error)

	// -------------------------------------------------------------------
	// Effort persistence
	// -------------------------------------------------------------------

	// SaveSegmentEffort upserts an effort by its natural key
	// (segment_id, activity_source_id, start_offset). Reimports of the
	// same source replace any previously recorded effort with the same
	// natural key.
	SaveSegmentEffort(ctx context.Context, e domain.SegmentEffort) error

	// DeleteEffortsForSource removes every effort that was discovered in
	// the given activity source. Called before re-running the matching
	// pass on a reimported source so stale efforts don't accumulate.
	DeleteEffortsForSource(ctx context.Context, sourceID domain.SourceID) error

	// AttachProviderEffortRef stamps the provider's effort id onto the
	// matcher-found effort for (segment, source) whose start time lies
	// within toleranceS seconds of startTime. Reports whether an effort
	// was found. Keeps the matcher canonical for GPS sources while still
	// linking each traversal back to the provider's record.
	AttachProviderEffortRef(ctx context.Context, segmentID domain.SegmentID, sourceID domain.SourceID, providerEffortExternalID string, startTime time.Time, toleranceS int) (bool, error)

	// ListEffortsForActivity returns every effort discovered in any source
	// of the activity. Used by the activity detail page.
	ListEffortsForActivity(ctx context.Context, id domain.ActivityID) ([]domain.SegmentEffort, error)

	// FindSimilarActivities returns the user's other activities over the same
	// route (shared segment efforts) as activityID, which starts near
	// (lat, lng) with the given distance and type.
	FindSimilarActivities(ctx context.Context, activityID domain.ActivityID, userID domain.UserID, lat, lng, distanceM float64, activityType string) ([]domain.SimilarActivity, error)

	// -------------------------------------------------------------------
	// Leaderboard reads (denormalized via rank columns)
	// -------------------------------------------------------------------

	// ListEffortsForSegment returns up to `limit` efforts on the segment,
	// sorted by elapsed_s ASC (fastest first). Offset enables pagination.
	// scope decides which leaderboard:
	//
	//   * PersonalOnly  - only efforts by `userID`
	//   * Instance      - all efforts (only valid for instance-scope segments
	//                     and Cairn-native private segments where the
	//                     caller IS the owner — adapters enforce)
	ListEffortsForSegment(
		ctx context.Context,
		segmentID domain.SegmentID,
		viewerUserID domain.UserID,
		scope LeaderboardScope,
		limit int,
		offset int,
	) ([]domain.SegmentEffort, error)

	// UserSegmentStats is the viewer's headline block: distinct segments
	// with efforts, efforts, records, external vs native.
	UserSegmentStats(ctx context.Context, userID domain.UserID) (domain.UserSegmentStats, error)

	// ListUserSegmentsByEffortCount pages the viewer's segments, most
	// attempted first; activityType "" = any.
	ListUserSegmentsByEffortCount(ctx context.Context, userID domain.UserID, activityType string, limit, offset int) ([]domain.UserSegmentListItem, error)

	// -------------------------------------------------------------------
	// Rank denormalization
	// -------------------------------------------------------------------

	// RecomputeRanksForSegment refreshes the denormalized rank columns
	// (personal_rank, instance_rank, is_personal_record, is_instance_record)
	// for every effort on the segment. Called after every effort write —
	// reimports invalidate ranks, since a new effort can dethrone or
	// reorder existing ones.
	//
	// Implementations use a window-function CTE to walk the leaderboard
	// in a single SQL statement; the use case wraps the call in a
	// transaction so callers that batch multiple segments share one tx.
	RecomputeRanksForSegment(ctx context.Context, segmentID domain.SegmentID) error

	// ---- authoring, browsing and leaderboards -------------------------

	// ViewerOwnsAccount reports whether the external account belongs to the viewer.
	ViewerOwnsAccount(ctx context.Context, viewer domain.UserID, account domain.ExternalAccountID) (bool, error)

	// CountEffortsForSegment returns how many efforts exist on the segment.
	CountEffortsForSegment(ctx context.Context, segmentID domain.SegmentID) (int, error)

	// ListSegments returns the segments visible to the viewer (their own
	// native ones, instance-scoped ones, mirrors of their accounts) with
	// the filter and sort applied, plus the total match count.
	ListSegments(ctx context.Context, viewer domain.UserID, f domain.SegmentListFilter) ([]domain.Segment, int, error)

	// SearchSegmentsInBBox returns visible segments whose bbox intersects box.
	SearchSegmentsInBBox(ctx context.Context, viewer domain.UserID, box BoundingBox, types []domain.ActivityType, includeInstance bool, limit int) ([]domain.Segment, error)

	// ListSegmentEffortsFiltered lists efforts on one segment (visibility is
	// the caller's job) with the query applied.
	ListSegmentEffortsFiltered(ctx context.Context, segmentID domain.SegmentID, q domain.SegmentEffortQuery) ([]domain.SegmentEffort, error)

	// SegmentLeaderboard ranks each athlete's best effort in the window.
	// mine is the viewer's own entry (nil when they have none).
	SegmentLeaderboard(ctx context.Context, segmentID domain.SegmentID, viewer domain.UserID, q domain.LeaderboardQuery) (entries []domain.LeaderboardEntry, mine *domain.LeaderboardEntry, err error)
}

// LeaderboardScope selects which efforts contribute to a leaderboard query.
type LeaderboardScope string

const (
	LeaderboardScopePersonalOnly LeaderboardScope = "personal"
	LeaderboardScopeInstance     LeaderboardScope = "instance"
)
