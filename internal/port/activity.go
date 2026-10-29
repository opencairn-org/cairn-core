package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ActivityYearStat is a per-year aggregate for the Stats page.
type ActivityYearStat struct {
	Year           int
	Count          int
	DistanceM      float64
	MovingS        float64
	ElevationGainM float64
}

// ActivityTotalsResult holds aggregate sums for the overview dashboard.
type ActivityTotalsResult struct {
	Count          int
	DistanceM      float64
	MovingS        float64
	ElevationGainM float64
}

// ActivityFacet is a (value, count) bucket for faceted filtering — e.g.
// {Value: "run", Count: 49}. Value is the raw stored enum string.
type ActivityFacet struct {
	Value string
	Count int
}

// StartPlaceCandidate identifies an activity awaiting reverse-geocoding of its
// start location, plus the source whose stream holds the first GPS point.
type StartPlaceCandidate struct {
	ActivityID            domain.ActivityID
	PrimaryStreamSourceID domain.SourceID
}

// ActivityListFilter narrows and sorts the activity feed. Empty filter fields
// match everything; nil pointers mean "any".
// SourceWorkerGroup is the sources one worker pool imported.
type SourceWorkerGroup struct {
	WorkerName       string
	Sources          int
	Accounts         int
	Users            int
	OldestImportedAt *time.Time
}

// UserFootprint is what an admin sees next to a user: how much they own.
type UserFootprint struct {
	Activities       int
	ExternalAccounts int
	StorageBytes     int64
	LastActivityAt   *time.Time
}

type ActivityListFilter struct {
	Type       string // activities.type, "" = any
	Discipline string // activities.discipline, "" = any
	// Sort: "date_desc" (default) | "date_asc" | "distance_desc" |
	// "duration_desc" | "elevation_desc" | "speed_desc".
	Sort string
	// From/To bound start_time to a half-open range [From, To). Zero values are
	// unbounded — used for the dashboard's "this week" / "last week" drill-down.
	From time.Time
	To   time.Time

	// Classification flags — tri-state: nil = any, true/false = must match.
	IsVirtual *bool
	IsEbike   *bool
	IsCommute *bool
	IsRace    *bool

	// Numeric ranges, all inclusive and in SI units. A nil bound is open.
	// Activities with a NULL value in the column never match a bounded range.
	DistanceMinM   *float64 // distance_m
	DistanceMaxM   *float64
	DurationMinS   *float64 // elapsed_duration_s
	DurationMaxS   *float64
	ElevationMinM  *float64 // elevation_gain_m
	ElevationMaxM  *float64
	AvgSpeedMinMps *float64 // avg_speed_mps
	AvgSpeedMaxMps *float64
	AvgHRMinBpm    *float64 // avg_heart_rate_bpm
	AvgHRMaxBpm    *float64
	AvgPowerMinW   *float64 // avg_power_w
	AvgPowerMaxW   *float64

	// GearIDs restricts to activities attached to any of these gear.
	GearIDs []domain.GearID

	Limit  int // <= 0 → default
	Offset int // page offset; 0 = first page
}

// ActivityRepo persists and retrieves the Activity aggregate.
//
// The aggregate has two parts: the merged Activity row and zero-or-more
// ActivitySource rows. ActivityRepo manages both because their consistency
// is jointly enforced — a merge that updates one usually updates the other.
//
// Read methods return domain.ErrNotFound when the requested entity does
// not exist. Adapters wrap the sentinel: `return Activity{}, fmt.Errorf("get activity %s: %w", id, domain.ErrNotFound)`.
type ActivityRepo interface {
	// -------------------------------------------------------------------
	// Activity (the merged aggregate root)
	// -------------------------------------------------------------------

	// GetActivity returns one Activity by ID. Soft-deleted activities
	// are included; callers filter by Activity.IsDeleted() when needed.
	GetActivity(ctx context.Context, id domain.ActivityID) (domain.Activity, error)

	// SaveActivity upserts the Activity row by ID. Used both for the
	// initial insert and for re-merge after sources change.
	SaveActivity(ctx context.Context, a domain.Activity) error

	// SoftDeleteActivity sets activities.deleted_at. Hard delete is a
	// separate maintenance operation (not exposed via the use-case API).
	SoftDeleteActivity(ctx context.Context, id domain.ActivityID, at time.Time) error

	// ListActivitiesForUser returns the user's activities whose start_time
	// falls within [start, end] (inclusive on both ends), ordered by
	// start_time ASC. Soft-deleted activities are excluded.
	//
	// Used by training-load compute and by any future dashboard view that
	// aggregates per-period metrics across activities. Callers that need
	// pagination should narrow the window; this method always returns the
	// full match set.
	ListActivitiesForUser(
		ctx context.Context,
		userID domain.UserID,
		start, end time.Time,
	) ([]domain.Activity, error)

	// ListRecentActivitiesForUser returns the user's newest `limit` activities
	// ordered by start_time DESC, regardless of age. Soft-deleted rows are
	// excluded. This is the count-based pagination the activity feed uses (the
	// time-windowed ListActivitiesForUser is for period aggregates). limit <= 0
	// falls back to a sane default.
	ListRecentActivitiesForUser(
		ctx context.Context,
		userID domain.UserID,
		limit int,
	) ([]domain.Activity, error)

	// ActivityTotals returns aggregate sums across the user's (non-deleted)
	// activities: count, total distance (m), total moving time (s), total
	// elevation gain (m). Used by the overview dashboard.
	ActivityTotals(ctx context.Context, userID domain.UserID) (ActivityTotalsResult, error)

	// SourceWorkerGroups counts active sources per importing worker pool.
	SourceWorkerGroups(ctx context.Context) ([]SourceWorkerGroup, error)

	// UserFootprints returns per-user admin counters for the given users
	// (missing users map to a zero footprint).
	UserFootprints(ctx context.Context, userIDs []domain.UserID) (map[domain.UserID]UserFootprint, error)

	// ActivityTotalsForRange returns totals for activities whose start_time
	// falls in [start, end) — the dashboard's this/last week.
	ActivityTotalsForRange(ctx context.Context, userID domain.UserID, start, end time.Time) (ActivityTotalsResult, error)

	// ActivityYearStats returns per-calendar-year totals (newest year first).
	ActivityYearStats(ctx context.Context, userID domain.UserID) ([]ActivityYearStat, error)

	// ActivityFacets returns the distinct type and discipline values the user
	// has, each with its count, plus the grand total. The facets CROSS-FILTER:
	// the type facet respects filter.Discipline and the discipline facet
	// respects filter.Type (but each ignores its own dimension, so the user can
	// still switch within it). This keeps the filter UI to valid combinations
	// only — e.g. picking "ride" leaves just ride_* disciplines. Soft-deleted
	// rows excluded; total is the unfiltered grand total.
	ActivityFacets(
		ctx context.Context,
		userID domain.UserID,
		filter ActivityListFilter,
	) (types, disciplines []ActivityFacet, total int, err error)

	// ListActivitiesFiltered returns a filtered + sorted page of the user's
	// activities AND the total count matching the filter (ignoring the limit),
	// for an accurate "N activities" summary. Soft-deleted rows excluded.
	ListActivitiesFiltered(
		ctx context.Context,
		userID domain.UserID,
		filter ActivityListFilter,
	) (activities []domain.Activity, matched int, err error)

	// ListFollowingFeed returns a reverse-chronological page of activities
	// owned by users the viewer follows (accepted edges), restricted to
	// non-private activities. Soft-deleted rows excluded.
	ListFollowingFeed(
		ctx context.Context,
		viewerID domain.UserID,
		limit, offset int,
	) ([]domain.Activity, error)

	// -------------------------------------------------------------------
	// ActivitySource (component of the aggregate)
	// -------------------------------------------------------------------

	// GetSource returns one ActivitySource by ID.
	GetSource(ctx context.Context, id domain.SourceID) (domain.ActivitySource, error)

	// GetSourceParsedRaw returns the source's normalized payload as stored
	// (activity_sources.parsed jsonb) — for the source-data viewer.
	GetSourceParsedRaw(ctx context.Context, id domain.SourceID) ([]byte, error)

	// ExistingExternalIDs returns the set of provider external_ids already
	// imported for an external account. Used by the full-sync "skip
	// already-present items" path.
	ExistingExternalIDs(ctx context.Context, provider string, accountID domain.ExternalAccountID) (map[string]struct{}, error)

	// CountSourcesOutOfDate counts the account's sources flagged
	// update_available, by reason (newer_parser, pool_changed, …).
	CountSourcesOutOfDate(ctx context.Context, accountID domain.ExternalAccountID) (map[string]int, error)

	// ListSourcesForActivity returns every non-detached source attached
	// to the activity. The order is deterministic: ImportedAt DESC so the
	// merge engine receives input in the expected sort order.
	ListSourcesForActivity(ctx context.Context, id domain.ActivityID) ([]domain.ActivitySource, error)

	// ListAllSourcesForActivity returns every source on the activity
	// INCLUDING detached ones, ordered ImportedAt DESC. Used by the
	// user-facing activity-manage view, which surfaces detached sources
	// for audit/recovery (the merge pipeline uses ListSourcesForActivity,
	// which excludes them).
	ListAllSourcesForActivity(ctx context.Context, id domain.ActivityID) ([]domain.ActivitySource, error)

	// SaveSource upserts a source by ID. Used by the ingest pipeline
	// when a worker delivers a new payload and by the preview-commit
	// flow when a dry-run reimport is applied.
	SaveSource(ctx context.Context, s domain.ActivitySource) error

	// ListSourceRecordsInBucket returns the lightweight match-feature view of
	// every non-detached source for a user with start_utc in [from, to) — the
	// candidate-generation query the re-clustering engine walks per
	// (user, UTC-day±margin). Sport is NOT filtered here (the matcher gates on
	// sport so synonyms/wildcards still compare). Ordered by source id for
	// deterministic clustering.
	ListSourceRecordsInBucket(ctx context.Context, userID domain.UserID, from, to time.Time) ([]domain.SourceMatchRecord, error)

	// ReassignSource moves a source to a different logical activity (the
	// reconciliation step assigns clusters to stable activity ids). No-op if the
	// source already belongs to newActivityID.
	ReassignSource(ctx context.Context, sourceID domain.SourceID, newActivityID domain.ActivityID) error

	// SetActivityMatchState records the matcher's confidence band + review flag
	// on an activity (re-cluster output).
	SetActivityMatchState(ctx context.Context, id domain.ActivityID, confidence string, needsReview bool) error

	// ListActivitiesNeedingReview returns the user's activities flagged
	// needs_review (medium-confidence merges), newest first — the review queue
	// (brief §7.3). Lightweight projection; does not load the full Activity.
	ListActivitiesNeedingReview(ctx context.Context, userID domain.UserID) ([]domain.ActivityReviewItem, error)

	// ClearNeedsReview clears the needs_review flag (user confirmed the merge).
	ClearNeedsReview(ctx context.Context, id domain.ActivityID) error

	// AddActivityRedirect records that a dissolved activity id now resolves to a
	// surviving one (merge). Idempotent on old_id.
	AddActivityRedirect(ctx context.Context, oldID, newID domain.ActivityID, reason string) error

	// ListMatchConstraints returns the user's manual matching decisions
	// (must_link / cannot_link) — fed into clustering as hard constraints.
	ListMatchConstraints(ctx context.Context, userID domain.UserID) ([]domain.MatchConstraint, error)

	// AddMatchConstraint records a manual matching decision. The pair is stored
	// in canonical order; a conflicting kind for the same pair is replaced.
	AddMatchConstraint(ctx context.Context, c domain.MatchConstraint) error

	// SetSourceRawBlob records the archived raw-file reference (S3 key +
	// content-type + size) on a source after a server-side upload archival.
	SetSourceRawBlob(ctx context.Context, id domain.SourceID, blobID, contentType string, sizeBytes int64) error

	// SetStartLocation writes the denormalised start coordinates and reverse-
	// geocoded place name for an activity. Managed out-of-band from the merge
	// engine so a re-merge never clobbers a resolved place. place == "" marks
	// the activity as "geocode attempted, no place found".
	SetStartLocation(ctx context.Context, id domain.ActivityID, lat, lng *float64, place string) error

	// SetStartCoords writes ONLY the start coordinates (not start_place), at
	// ingest time so reverse-geocoding has a point immediately. Leaving
	// start_place NULL keeps the async geocoder's work queue intact.
	SetStartCoords(ctx context.Context, id domain.ActivityID, lat, lng float64) error

	// ListActivitiesMissingStartPlace returns up to `limit` non-deleted
	// activities that have a primary stream but no start_place yet, newest
	// first. Drives the geocode backfiller's work queue.
	ListActivitiesMissingStartPlace(ctx context.Context, limit int) ([]StartPlaceCandidate, error)

	// DetachSource marks a source as detached and bumps the parent
	// activity's updated_at. Detached sources stay in storage for audit
	// but no longer contribute to the merge. The caller is responsible
	// for invoking RecomputeActivityFromSources afterwards.
	DetachSource(ctx context.Context, id domain.SourceID, reason string, at time.Time) error

	// SetSourceReimportStatus flips a source's reimport_status (+reason),
	// e.g. to 'updating' when a user-initiated re-fetch/re-parse job is
	// published. The ingest pipeline resets it to 'current' on the result.
	SetSourceReimportStatus(ctx context.Context, id domain.SourceID, status domain.ReimportStatus, reason string) error

	// -------------------------------------------------------------------
	// Identity lookup (used by the ingest pipeline)
	// -------------------------------------------------------------------

	// FindSourceByExternalID resolves a source's identity: exact match on
	// (provider, external_account_id, external_id). Returns domain.ErrNotFound
	// when no such source exists. Same-activity GROUPING is the matcher's job,
	// not this lookup's.
	FindSourceByExternalID(
		ctx context.Context,
		provider string,
		externalAccountID *domain.ExternalAccountID,
		externalID string,
	) (domain.ActivitySource, error)
}
