package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ActivityRepo persists the Activity aggregate (activities + activity_sources).
type ActivityRepo struct {
	pool *pgxpool.Pool
}

// NewActivityRepo wires the repository onto an existing pool.
func NewActivityRepo(pool *pgxpool.Pool) *ActivityRepo {
	return &ActivityRepo{pool: pool}
}

// ---------------------------------------------------------------------------
// Activity (the merged row)
// ---------------------------------------------------------------------------

// activityColumns lists every column on the activities table. The order
// is fixed: scanActivityRow relies on it.
const activityColumns = `
	id, user_id,
	type, discipline, is_virtual, is_ebike, is_commute, is_race, custom_subtype,
	title, description,
	start_time, end_time, elapsed_duration_s, moving_duration_s, timezone,
	distance_m, elevation_gain_m, elevation_loss_m, min_elevation_m, max_elevation_m,
	avg_speed_mps, max_speed_mps,
	avg_heart_rate_bpm, max_heart_rate_bpm,
	avg_power_w, max_power_w, normalized_power_w,
	avg_cadence, max_cadence,
	avg_temperature_c, min_temperature_c, max_temperature_c,
	calories_kcal, tss, intensity_factor,
	pool_length_m, total_strokes,
	merge_provenance,
	primary_stream_source_id,
	merged_at,
	gear_id, tags, privacy,
	created_at, updated_at, deleted_at,
	start_lat, start_lng, start_place,
	hidden_by_admin,
	gear_set_by
`

// GetActivity reads one activities row by ID.
func (r *ActivityRepo) GetActivity(ctx context.Context, id domain.ActivityID) (domain.Activity, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT `+activityColumns+` FROM activities WHERE id = $1`,
		id.UUID(),
	)
	a, err := scanActivityRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Activity{}, fmt.Errorf("get activity %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Activity{}, fmt.Errorf("get activity %s: %w", id, err)
	}
	return a, nil
}

// SaveActivity upserts an activities row by ID. Callers in the same
// transaction must also persist any source referenced by
// PrimaryStreamSourceID — the FK is DEFERRABLE INITIALLY DEFERRED so
// order within the transaction does not matter; validity is checked at
// commit.
func (r *ActivityRepo) SaveActivity(ctx context.Context, a domain.Activity) error {
	db := dbtx(ctx, r.pool)

	provenanceJSON, err := encodeProvenance(a.MergeProvenance)
	if err != nil {
		return fmt.Errorf("encode merge_provenance: %w", err)
	}

	_, err = db.Exec(ctx, activitySaveSQL, activitySaveArgs(a, provenanceJSON)...)
	if err != nil {
		return fmt.Errorf("save activity %s: %w", a.ID, err)
	}
	return nil
}

// activitySaveArgs builds the activitySaveSQL bind parameters.
func activitySaveArgs(a domain.Activity, provenanceJSON []byte) []any {
	var disciplineParam any
	if a.Discipline != "" && a.Discipline != domain.DisciplineNone {
		disciplineParam = string(a.Discipline)
	}

	var primaryStreamParam any
	if a.PrimaryStreamSourceID != nil {
		primaryStreamParam = a.PrimaryStreamSourceID.UUID()
	}

	var gearParam any
	if a.GearID != nil {
		gearParam = a.GearID.UUID()
	}

	var deletedAtParam any
	if a.DeletedAt != nil {
		deletedAtParam = *a.DeletedAt
	}

	// activities.tags is NOT NULL. A nil Go slice binds as SQL NULL and
	// violates the constraint — the column DEFAULT '{}' does not apply when
	// the INSERT supplies the column explicitly. Coalesce to an empty array.
	tags := a.Tags
	if tags == nil {
		tags = []string{}
	}

	return []any{
		a.ID.UUID(), a.UserID.UUID(),
		string(a.Type), disciplineParam, a.IsVirtual, a.IsEbike, a.IsCommute, a.IsRace, a.CustomSubtype,
		a.Title, a.Description,
		a.StartTime, a.EndTime, int64(a.ElapsedDuration / time.Second), int64(a.MovingDuration / time.Second), a.Timezone,
		a.Summary.DistanceM, a.Summary.ElevationGainM, a.Summary.ElevationLossM, a.Summary.MinElevationM, a.Summary.MaxElevationM,
		a.Summary.AvgSpeedMps, a.Summary.MaxSpeedMps,
		a.Summary.AvgHeartRateBpm, a.Summary.MaxHeartRateBpm,
		a.Summary.AvgPowerW, a.Summary.MaxPowerW, a.Summary.NormalizedPowerW,
		a.Summary.AvgCadence, a.Summary.MaxCadence,
		a.Summary.AvgTemperatureC, a.Summary.MinTemperatureC, a.Summary.MaxTemperatureC,
		a.Summary.CaloriesKcal, a.Summary.TSS, a.Summary.IntensityFactor,
		a.Summary.PoolLengthM, a.Summary.TotalStrokes,
		provenanceJSON,
		primaryStreamParam,
		a.MergedAt,
		gearParam, tags, string(a.Privacy),
		nullableTime(a.CreatedAt), deletedAtParam,
		string(gearSetByOrMerge(a.GearSetBy)),
	}
}

// gearIDParams converts typed gear ids to the uuid slice pgx binds as uuid[].
func gearIDParams(ids []domain.GearID) []uuid.UUID {
	out := make([]uuid.UUID, len(ids))
	for i, g := range ids {
		out[i] = g.UUID()
	}
	return out
}

// activitySaveSQL upserts one activities row by id.
const activitySaveSQL = `
		INSERT INTO activities (
			id, user_id,
			type, discipline, is_virtual, is_ebike, is_commute, is_race, custom_subtype,
			title, description,
			start_time, end_time, elapsed_duration_s, moving_duration_s, timezone,
			distance_m, elevation_gain_m, elevation_loss_m, min_elevation_m, max_elevation_m,
			avg_speed_mps, max_speed_mps,
			avg_heart_rate_bpm, max_heart_rate_bpm,
			avg_power_w, max_power_w, normalized_power_w,
			avg_cadence, max_cadence,
			avg_temperature_c, min_temperature_c, max_temperature_c,
			calories_kcal, tss, intensity_factor,
			pool_length_m, total_strokes,
			merge_provenance,
			primary_stream_source_id,
			merged_at,
			gear_id, tags, privacy,
			created_at, updated_at, deleted_at,
			gear_set_by
		) VALUES (
			$1, $2,
			$3, $4, $5, $6, $7, $8, $9,
			$10, $11,
			$12, $13, $14, $15, $16,
			$17, $18, $19, $20, $21,
			$22, $23,
			$24, $25,
			$26, $27, $28,
			$29, $30,
			$31, $32, $33,
			$34, $35, $36,
			$37, $38,
			$39,
			$40,
			$41,
			$42, $43, $44,
			COALESCE($45, now()), now(), $46,
			$47
		)
		ON CONFLICT (id) DO UPDATE SET
			type = EXCLUDED.type,
			discipline = EXCLUDED.discipline,
			is_virtual = EXCLUDED.is_virtual,
			is_ebike = EXCLUDED.is_ebike,
			is_commute = EXCLUDED.is_commute,
			is_race = EXCLUDED.is_race,
			custom_subtype = EXCLUDED.custom_subtype,
			title = EXCLUDED.title,
			description = EXCLUDED.description,
			start_time = EXCLUDED.start_time,
			end_time = EXCLUDED.end_time,
			elapsed_duration_s = EXCLUDED.elapsed_duration_s,
			moving_duration_s = EXCLUDED.moving_duration_s,
			timezone = EXCLUDED.timezone,
			distance_m = EXCLUDED.distance_m,
			elevation_gain_m = EXCLUDED.elevation_gain_m,
			elevation_loss_m = EXCLUDED.elevation_loss_m,
			min_elevation_m = EXCLUDED.min_elevation_m,
			max_elevation_m = EXCLUDED.max_elevation_m,
			avg_speed_mps = EXCLUDED.avg_speed_mps,
			max_speed_mps = EXCLUDED.max_speed_mps,
			avg_heart_rate_bpm = EXCLUDED.avg_heart_rate_bpm,
			max_heart_rate_bpm = EXCLUDED.max_heart_rate_bpm,
			avg_power_w = EXCLUDED.avg_power_w,
			max_power_w = EXCLUDED.max_power_w,
			normalized_power_w = EXCLUDED.normalized_power_w,
			avg_cadence = EXCLUDED.avg_cadence,
			max_cadence = EXCLUDED.max_cadence,
			avg_temperature_c = EXCLUDED.avg_temperature_c,
			min_temperature_c = EXCLUDED.min_temperature_c,
			max_temperature_c = EXCLUDED.max_temperature_c,
			calories_kcal = EXCLUDED.calories_kcal,
			tss = EXCLUDED.tss,
			intensity_factor = EXCLUDED.intensity_factor,
			pool_length_m = EXCLUDED.pool_length_m,
			total_strokes = EXCLUDED.total_strokes,
			merge_provenance = EXCLUDED.merge_provenance,
			primary_stream_source_id = EXCLUDED.primary_stream_source_id,
			merged_at = EXCLUDED.merged_at,
			gear_id = EXCLUDED.gear_id,
			gear_set_by = EXCLUDED.gear_set_by,
			tags = EXCLUDED.tags,
			privacy = EXCLUDED.privacy,
			-- created_at: never overwritten (preserves original creation time)
			updated_at = now(),
			deleted_at = EXCLUDED.deleted_at
	`

// SoftDeleteActivity sets activities.deleted_at.
func (r *ActivityRepo) SoftDeleteActivity(ctx context.Context, id domain.ActivityID, at time.Time) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE activities SET deleted_at = $1, updated_at = now() WHERE id = $2 AND deleted_at IS NULL`,
		at, id.UUID(),
	)
	if err != nil {
		return fmt.Errorf("soft delete activity %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		// Either the row does not exist or is already deleted. We treat
		// both as non-errors — soft-delete is idempotent.
		return nil
	}
	return nil
}

// SetSourceRawBlob records the archived raw-file reference on a source.
func (r *ActivityRepo) SetSourceRawBlob(
	ctx context.Context,
	id domain.SourceID,
	blobID, contentType string,
	sizeBytes int64,
) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE activity_sources
		    SET raw_blob_id = $2, raw_content_type = $3, raw_size_bytes = $4, updated_at = now()
		  WHERE id = $1`,
		id.UUID(), blobID, contentType, sizeBytes,
	)
	if err != nil {
		return fmt.Errorf("set source raw blob %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set source raw blob %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

// SetStartLocation writes the denormalised start coordinates and place name.
// Kept separate from SaveActivity so a re-merge never overwrites a resolved
// place (SaveActivity's ON CONFLICT does not list these columns). Pass
// place == "" to mark an activity as "geocode attempted, no place" so the
// backfiller stops reconsidering it.
func (r *ActivityRepo) SetStartLocation(
	ctx context.Context,
	id domain.ActivityID,
	lat, lng *float64,
	place string,
) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE activities
		    SET start_lat = $2, start_lng = $3, start_place = $4, updated_at = now()
		  WHERE id = $1`,
		id.UUID(), lat, lng, place,
	)
	if err != nil {
		return fmt.Errorf("set start location %s: %w", id, err)
	}
	return nil
}

// SetStartCoords writes ONLY start_lat/start_lng (not start_place), at ingest
// time, so stage-3 geo-dedup can find existing activities by location.
func (r *ActivityRepo) SetStartCoords(ctx context.Context, id domain.ActivityID, lat, lng float64) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE activities SET start_lat = $2, start_lng = $3, updated_at = now() WHERE id = $1`,
		id.UUID(), lat, lng,
	)
	if err != nil {
		return fmt.Errorf("set start coords %s: %w", id, err)
	}
	return nil
}

// ListActivitiesMissingStartPlace returns up to `limit` non-deleted activities
// that have a primary stream but no start_place yet (start_place IS NULL),
// newest first. Used by the geocode backfiller's work queue.
func (r *ActivityRepo) ListActivitiesMissingStartPlace(
	ctx context.Context,
	limit int,
) ([]port.StartPlaceCandidate, error) {
	if limit <= 0 {
		limit = 50
	}
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT id, primary_stream_source_id
		   FROM activities
		  WHERE start_place IS NULL
		    AND primary_stream_source_id IS NOT NULL
		    AND deleted_at IS NULL
		  ORDER BY start_time DESC
		  LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list activities missing start place: %w", err)
	}
	defer rows.Close()

	var out []port.StartPlaceCandidate
	for rows.Next() {
		var id, srcID uuid.UUID
		if err := rows.Scan(&id, &srcID); err != nil {
			return nil, fmt.Errorf("scan start-place candidate: %w", err)
		}
		out = append(out, port.StartPlaceCandidate{
			ActivityID:            domain.ActivityID(id),
			PrimaryStreamSourceID: domain.SourceID(srcID),
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// ActivitySource
// ---------------------------------------------------------------------------

const sourceColumns = `
	id, activity_id, user_id,
	provider, external_account_id, external_id,
	source_worker_name, source_worker_version, source_worker_package,
	raw_blob_id, raw_content_type, raw_size_bytes,
	parsed,
	status, status_reason,
	reimport_status, reimport_status_reason,
	imported_at, last_reimported_at, updated_at,
	source_worker_release
`

// GetSource reads one activity_sources row by ID.
func (r *ActivityRepo) GetSourceParsedRaw(ctx context.Context, id domain.SourceID) ([]byte, error) {
	db := dbtx(ctx, r.pool)
	var raw []byte
	err := db.QueryRow(ctx, `SELECT parsed FROM activity_sources WHERE id = $1`, id.UUID()).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get source parsed %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get source parsed %s: %w", id, err)
	}
	return raw, nil
}

func (r *ActivityRepo) GetSource(ctx context.Context, id domain.SourceID) (domain.ActivitySource, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT `+sourceColumns+` FROM activity_sources WHERE id = $1`,
		id.UUID(),
	)
	s, err := scanSourceRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ActivitySource{}, fmt.Errorf("get source %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return domain.ActivitySource{}, fmt.Errorf("get source %s: %w", id, err)
	}
	return s, nil
}

// ListSourcesForActivity returns every non-detached source on the activity,
// ordered ImportedAt DESC so the merge engine receives them in the
// expected order.
func (r *ActivityRepo) ListSourcesForActivity(ctx context.Context, id domain.ActivityID) ([]domain.ActivitySource, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT `+sourceColumns+`
		   FROM activity_sources
		  WHERE activity_id = $1
		    AND status != 'detached'
		  ORDER BY imported_at DESC`,
		id.UUID(),
	)
	if err != nil {
		return nil, fmt.Errorf("list sources for activity %s: %w", id, err)
	}
	return pgCollectRows(rows, scanSourceRow, "scan source row", "iterate source rows")
}

// ListAllSourcesForActivity returns every source on the activity INCLUDING
// detached ones, ordered ImportedAt DESC. The user-facing manage view shows
// detached sources for audit; the merge pipeline uses ListSourcesForActivity.
func (r *ActivityRepo) ListAllSourcesForActivity(ctx context.Context, id domain.ActivityID) ([]domain.ActivitySource, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT `+sourceColumns+`
		   FROM activity_sources
		  WHERE activity_id = $1
		  ORDER BY imported_at DESC`,
		id.UUID(),
	)
	if err != nil {
		return nil, fmt.Errorf("list all sources for activity %s: %w", id, err)
	}
	return pgCollectRows(rows, scanSourceRow, "scan source row", "iterate source rows")
}

// SaveSource upserts an activity_sources row by ID.
func (r *ActivityRepo) SaveSource(ctx context.Context, s domain.ActivitySource) error {
	db := dbtx(ctx, r.pool)

	parsedJSON, err := encodePayload(s.Parsed)
	if err != nil {
		return fmt.Errorf("encode parsed: %w", err)
	}

	_, err = db.Exec(ctx, sourceSaveSQL, sourceSaveArgs(s, parsedJSON)...)
	if err != nil {
		return fmt.Errorf("save source %s: %w", s.ID, err)
	}
	return nil
}

// sourceSaveArgs builds the sourceSaveSQL bind parameters.
func sourceSaveArgs(s domain.ActivitySource, parsedJSON []byte) []any {
	var externalAccountParam any
	if s.ExternalAccountID != nil {
		externalAccountParam = s.ExternalAccountID.UUID()
	}

	var lastReimportedParam any
	if s.LastReimportedAt != nil {
		lastReimportedParam = *s.LastReimportedAt
	}

	// Derived match-feature columns (Phase 2): a projection of Parsed (+ start
	// coords from the stream) the matcher blocks/scores on. Recomputed here on
	// every save so they can never drift from the raw payload.
	mf := sourceMatchFeatures(s)

	return []any{
		s.ID.UUID(), s.ActivityID.UUID(), s.UserID.UUID(),
		s.Provider, externalAccountParam, s.ExternalID,
		s.SourceWorkerName, s.SourceWorkerVersion, s.SourceWorkerPackage,
		s.RawBlobID, s.RawContentType, s.RawSizeBytes,
		parsedJSON,
		string(s.Status), s.StatusReason,
		string(s.ReimportStatus), s.ReimportStatusReason,
		nullableTime(s.ImportedAt), lastReimportedParam,
		mf.sportClass, nullableTime(mf.startUTC), mf.distanceM, mf.movingS, mf.elapsedS, mf.startLat, mf.startLng,
		s.SourceWorkerRelease,
	}
}

// sourceSaveSQL upserts one activity_sources row by id.
const sourceSaveSQL = `
		INSERT INTO activity_sources (
			id, activity_id, user_id,
			provider, external_account_id, external_id,
			source_worker_name, source_worker_version, source_worker_package,
			raw_blob_id, raw_content_type, raw_size_bytes,
			parsed,
			status, status_reason,
			reimport_status, reimport_status_reason,
			imported_at, last_reimported_at, updated_at,
			sport_class, start_utc, distance_m, moving_s, elapsed_s, start_lat, start_lng,
			source_worker_release
		) VALUES (
			$1, $2, $3,
			$4, $5, $6,
			$7, $8, $9,
			$10, $11, $12,
			$13,
			$14, $15,
			$16, $17,
			COALESCE($18, now()), $19, now(),
			$20, $21, $22, $23, $24, $25, $26,
			$27
		)
		ON CONFLICT (id) DO UPDATE SET
			-- activity_id, user_id, provider, external_account_id, external_id:
			-- identity columns; never overwritten on reimport
			source_worker_name = EXCLUDED.source_worker_name,
			source_worker_version = EXCLUDED.source_worker_version,
			source_worker_package = EXCLUDED.source_worker_package,
			source_worker_release = EXCLUDED.source_worker_release,
			raw_blob_id = EXCLUDED.raw_blob_id,
			raw_content_type = EXCLUDED.raw_content_type,
			raw_size_bytes = EXCLUDED.raw_size_bytes,
			parsed = EXCLUDED.parsed,
			status = EXCLUDED.status,
			status_reason = EXCLUDED.status_reason,
			reimport_status = EXCLUDED.reimport_status,
			reimport_status_reason = EXCLUDED.reimport_status_reason,
			last_reimported_at = EXCLUDED.last_reimported_at,
			updated_at = now(),
			sport_class = EXCLUDED.sport_class,
			start_utc = EXCLUDED.start_utc,
			distance_m = EXCLUDED.distance_m,
			moving_s = EXCLUDED.moving_s,
			elapsed_s = EXCLUDED.elapsed_s,
			start_lat = COALESCE(EXCLUDED.start_lat, activity_sources.start_lat),
			start_lng = COALESCE(EXCLUDED.start_lng, activity_sources.start_lng)
	`

// sourceMF is the derived match-feature projection written alongside a source.
type sourceMF struct {
	sportClass string
	startUTC   time.Time
	distanceM  *float64
	movingS    int64
	elapsedS   int64
	startLat   *float64
	startLng   *float64
}

// sourceMatchFeatures derives the blocking/scoring columns from a source's
// parsed payload (+ stream-derived start coords). Keeping this in one place
// guarantees the denormalized columns stay a faithful projection of `parsed`.
func sourceMatchFeatures(s domain.ActivitySource) sourceMF {
	return sourceMF{
		sportClass: string(s.Parsed.Type),
		startUTC:   s.Parsed.StartTime.UTC(),
		distanceM:  s.Parsed.Summary.DistanceM,
		movingS:    int64(s.Parsed.MovingDuration.Seconds()),
		elapsedS:   int64(s.Parsed.ElapsedDuration.Seconds()),
		startLat:   s.StartLat,
		startLng:   s.StartLng,
	}
}

// ListSourceRecordsInBucket returns the lightweight match-feature view of every
// non-detached source record for a user with start_utc in [from, to). The
// candidate-generation query the re-clustering engine (Phase 3/4) walks per
// (user, UTC-day±margin); sport is NOT filtered (the matcher gates on it so
// synonyms/wildcards still compare). Ordered by source id for deterministic
// clustering tie-breaks.
func (r *ActivityRepo) ListSourceRecordsInBucket(ctx context.Context, userID domain.UserID, from, to time.Time) ([]domain.SourceMatchRecord, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx, `
		SELECT id, activity_id, user_id, provider, external_account_id, external_id,
		       sport_class, start_utc, distance_m, moving_s, elapsed_s,
		       start_lat, start_lng, status
		  FROM activity_sources
		 WHERE user_id = $1
		   AND start_utc >= $2
		   AND start_utc < $3
		   AND status != 'detached'
		 ORDER BY id`,
		userID.UUID(), from.UTC(), to.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("list source records in bucket: %w", err)
	}
	return pgCollectRows(rows, scanSourceMatchRecord, "scan match record", "iterate match records")
}

// scanSourceMatchRecord scans one ListSourceRecordsInBucket row.
func scanSourceMatchRecord(row rowScanner) (domain.SourceMatchRecord, error) {
	var (
		id, activityID, uid uuid.UUID
		extAcct             *uuid.UUID
		provider, extID     string
		sport               string
		startUTC            time.Time
		distanceM           *float64
		movingS, elapsedS   int64
		startLat, startLng  *float64
		status              string
	)
	if err := row.Scan(&id, &activityID, &uid, &provider, &extAcct, &extID,
		&sport, &startUTC, &distanceM, &movingS, &elapsedS,
		&startLat, &startLng, &status); err != nil {
		return domain.SourceMatchRecord{}, err
	}
	rec := domain.SourceMatchRecord{
		SourceID:   domain.SourceID(id),
		ActivityID: domain.ActivityID(activityID),
		UserID:     domain.UserID(uid),
		Provider:   provider,
		ExternalID: extID,
		SportClass: sport,
		StartUTC:   startUTC,
		DistanceM:  distanceM,
		MovingS:    movingS,
		ElapsedS:   elapsedS,
		StartLat:   startLat,
		StartLng:   startLng,
		Status:     domain.SourceStatus(status),
	}
	if extAcct != nil {
		ea := domain.ExternalAccountID(*extAcct)
		rec.ExternalAccountID = &ea
	}
	return rec, nil
}

// ReassignSource moves a source to a different logical activity.
func (r *ActivityRepo) ReassignSource(ctx context.Context, sourceID domain.SourceID, newActivityID domain.ActivityID) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE activity_sources SET activity_id = $1, updated_at = now() WHERE id = $2`,
		newActivityID.UUID(), sourceID.UUID(),
	)
	if err != nil {
		return fmt.Errorf("reassign source %s: %w", sourceID, err)
	}
	return nil
}

// SetActivityMatchState records the matcher confidence band + review flag.
func (r *ActivityRepo) SetActivityMatchState(ctx context.Context, id domain.ActivityID, confidence string, needsReview bool) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE activities SET match_confidence = $1, needs_review = $2 WHERE id = $3`,
		confidence, needsReview, id.UUID(),
	)
	if err != nil {
		return fmt.Errorf("set match state %s: %w", id, err)
	}
	return nil
}

// ListActivitiesNeedingReview returns the user's needs_review activities, newest
// first (the confidence-band review queue).
func (r *ActivityRepo) ListActivitiesNeedingReview(ctx context.Context, userID domain.UserID) ([]domain.ActivityReviewItem, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT id, title, type, start_time, match_confidence
		   FROM activities
		  WHERE user_id = $1 AND needs_review AND deleted_at IS NULL
		  ORDER BY start_time DESC`,
		userID.UUID(),
	)
	if err != nil {
		return nil, fmt.Errorf("list activities needing review: %w", err)
	}
	defer rows.Close()
	var out []domain.ActivityReviewItem
	for rows.Next() {
		var (
			id        uuid.UUID
			title, ty string
			start     time.Time
			conf      string
		)
		if err := rows.Scan(&id, &title, &ty, &start, &conf); err != nil {
			return nil, fmt.Errorf("scan review item: %w", err)
		}
		out = append(out, domain.ActivityReviewItem{
			ID:              domain.ActivityID(id),
			Title:           title,
			Type:            domain.ActivityType(ty),
			StartTime:       start,
			MatchConfidence: conf,
		})
	}
	return out, rows.Err()
}

// ClearNeedsReview clears the needs_review flag (user confirmed the merge).
func (r *ActivityRepo) ClearNeedsReview(ctx context.Context, id domain.ActivityID) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx, `UPDATE activities SET needs_review = false WHERE id = $1`, id.UUID())
	if err != nil {
		return fmt.Errorf("clear needs_review %s: %w", id, err)
	}
	return nil
}

// AddActivityRedirect records a dissolved-activity → surviving-activity redirect.
func (r *ActivityRepo) AddActivityRedirect(ctx context.Context, oldID, newID domain.ActivityID, reason string) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`INSERT INTO activity_id_redirects (old_id, new_id, reason)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (old_id) DO UPDATE SET new_id = EXCLUDED.new_id, reason = EXCLUDED.reason`,
		oldID.UUID(), newID.UUID(), reason,
	)
	if err != nil {
		return fmt.Errorf("add activity redirect %s→%s: %w", oldID, newID, err)
	}
	return nil
}

// ListMatchConstraints returns the user's manual matching decisions.
func (r *ActivityRepo) ListMatchConstraints(ctx context.Context, userID domain.UserID) ([]domain.MatchConstraint, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT id, user_id, source_a, source_b, kind, reason, created_at
		   FROM match_constraints WHERE user_id = $1 ORDER BY id`,
		userID.UUID(),
	)
	if err != nil {
		return nil, fmt.Errorf("list match constraints: %w", err)
	}
	defer rows.Close()
	var out []domain.MatchConstraint
	for rows.Next() {
		var (
			id, uid, sa, sb uuid.UUID
			kind, reason    string
			createdAt       time.Time
		)
		if err := rows.Scan(&id, &uid, &sa, &sb, &kind, &reason, &createdAt); err != nil {
			return nil, fmt.Errorf("scan match constraint: %w", err)
		}
		out = append(out, domain.MatchConstraint{
			ID:        domain.MatchConstraintID(id),
			UserID:    domain.UserID(uid),
			SourceA:   domain.SourceID(sa),
			SourceB:   domain.SourceID(sb),
			Kind:      domain.ConstraintKind(kind),
			Reason:    reason,
			CreatedAt: createdAt,
		})
	}
	return out, rows.Err()
}

// AddMatchConstraint records a manual matching decision in canonical pair order.
func (r *ActivityRepo) AddMatchConstraint(ctx context.Context, c domain.MatchConstraint) error {
	db := dbtx(ctx, r.pool)
	a, b := c.SourceA, c.SourceB
	if a.String() > b.String() {
		a, b = b, a
	}
	id := c.ID
	if id == (domain.MatchConstraintID{}) {
		id = domain.MatchConstraintID(uuid.New())
	}
	_, err := db.Exec(ctx,
		`INSERT INTO match_constraints (id, user_id, source_a, source_b, kind, reason)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (source_a, source_b)
		 DO UPDATE SET kind = EXCLUDED.kind, reason = EXCLUDED.reason`,
		id.UUID(), c.UserID.UUID(), a.UUID(), b.UUID(), string(c.Kind), c.Reason,
	)
	if err != nil {
		return fmt.Errorf("add match constraint: %w", err)
	}
	return nil
}

// DetachSource marks the source as detached. The caller should follow up
// with RecomputeActivityFromSources on the parent activity.
func (r *ActivityRepo) DetachSource(ctx context.Context, id domain.SourceID, reason string, at time.Time) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE activity_sources
		    SET status = 'detached',
		        status_reason = $1,
		        updated_at = $2
		  WHERE id = $3`,
		reason, at, id.UUID(),
	)
	if err != nil {
		return fmt.Errorf("detach source %s: %w", id, err)
	}
	return nil
}

// SetSourceReimportStatus updates only the reimport_status (+reason) of a
// source — used by RequestSourceReimport to flip a source to 'updating' before
// a re-fetch/re-parse job is published; the ingest pipeline flips it back to
// 'current' when the result lands.
func (r *ActivityRepo) SetSourceReimportStatus(ctx context.Context, id domain.SourceID, status domain.ReimportStatus, reason string) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE activity_sources
		    SET reimport_status = $1, reimport_status_reason = $2, updated_at = now()
		  WHERE id = $3`,
		string(status), reason, id.UUID(),
	)
	if err != nil {
		return fmt.Errorf("set reimport status for source %s: %w", id, err)
	}
	return nil
}

// MarkSourcesOutOfDate flags the provider+package sources parsed by an older
// version than live (docs/adr/0005) as update_available. The reason says
// what helps: 'newer_parser' (same dataset, re-parse from the archive) or
// 'newer_dataset' (re-fetch). Idempotent. Sources of another package, or
// with a non-N.M version, are never touched.
func (r *ActivityRepo) MarkSourcesOutOfDate(
	ctx context.Context,
	provider, pkg string,
	live domain.ParserVersion,
) (int, error) {
	if provider == "" || pkg == "" {
		return 0, nil
	}
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE activity_sources
		    SET reimport_status        = 'update_available',
		        reimport_status_reason = CASE
		            WHEN (cairn_parser_version(source_worker_version))[1] < $3 THEN 'newer_dataset'
		            ELSE 'newer_parser' END
		  WHERE provider = $1
		    AND source_worker_package = $2
		    AND reimport_status IN ('current', 'update_available')
		    AND cairn_parser_version(source_worker_version) < ARRAY[$3::int, $4::int]
		    AND reimport_status_reason IS DISTINCT FROM CASE
		            WHEN (cairn_parser_version(source_worker_version))[1] < $3 THEN 'newer_dataset'
		            ELSE 'newer_parser' END`,
		provider, pkg, live.Dataset, live.Parser,
	)
	if err != nil {
		return 0, fmt.Errorf("mark sources out-of-date for %s/%s: %w", provider, pkg, err)
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------------
// Dedup
// ---------------------------------------------------------------------------

// ExistingExternalIDs returns the provider external_ids already imported for
// an external account (any non-detached source).
func (r *ActivityRepo) ExistingExternalIDs(ctx context.Context, provider string, accountID domain.ExternalAccountID) (map[string]struct{}, error) {
	rows, err := dbtx(ctx, r.pool).Query(ctx,
		`SELECT external_id FROM activity_sources
		 WHERE provider = $1 AND external_account_id = $2 AND status <> 'detached'`,
		provider, accountID.UUID())
	if err != nil {
		return nil, fmt.Errorf("existing external ids: %w", err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// FindSourceByExternalID implements dedup stage 1.
func (r *ActivityRepo) FindSourceByExternalID(
	ctx context.Context,
	provider string,
	externalAccountID *domain.ExternalAccountID,
	externalID string,
) (domain.ActivitySource, error) {
	db := dbtx(ctx, r.pool)

	var accountParam any
	if externalAccountID != nil {
		accountParam = externalAccountID.UUID()
	}

	// External_account_id may be NULL for manual uploads — the UNIQUE
	// constraint in migration 5 treats NULL as distinct in Postgres, but
	// dedup must treat (provider, NULL, external_id) as a single bucket
	// per user. We disambiguate via IS NOT DISTINCT FROM.
	row := db.QueryRow(ctx,
		`SELECT `+sourceColumns+`
		   FROM activity_sources
		  WHERE provider = $1
		    AND external_account_id IS NOT DISTINCT FROM $2
		    AND external_id = $3
		    AND status != 'detached'
		  LIMIT 1`,
		provider, accountParam, externalID,
	)
	s, err := scanSourceRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ActivitySource{}, fmt.Errorf("find source by external id: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.ActivitySource{}, fmt.Errorf("find source by external id: %w", err)
	}
	return s, nil
}

// ListActivitiesForUser returns activities for the user within [start, end]
// ordered by start_time ASC. Soft-deleted rows are excluded.
func (r *ActivityRepo) ListActivitiesForUser(
	ctx context.Context,
	userID domain.UserID,
	start, end time.Time,
) ([]domain.Activity, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT `+activityColumns+`
		   FROM activities
		  WHERE user_id = $1
		    AND start_time BETWEEN $2 AND $3
		    AND deleted_at IS NULL
		  ORDER BY start_time ASC`,
		userID.UUID(), start, end,
	)
	if err != nil {
		return nil, fmt.Errorf("list activities for %s: %w", userID, err)
	}
	return pgCollectRows(rows, scanActivityRow, "scan activity row", "iterate user activities")
}

// ListRecentActivitiesForUser returns the user's newest `limit` activities
// ordered by start_time DESC, regardless of age (count-based pagination for
// the feed). Soft-deleted rows excluded.
func (r *ActivityRepo) ListRecentActivitiesForUser(
	ctx context.Context,
	userID domain.UserID,
	limit int,
) ([]domain.Activity, error) {
	if limit <= 0 {
		limit = 50
	}
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT `+activityColumns+`
		   FROM activities
		  WHERE user_id = $1
		    AND deleted_at IS NULL
		  ORDER BY start_time DESC
		  LIMIT $2`,
		userID.UUID(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list recent activities for %s: %w", userID, err)
	}
	return pgCollectRows(rows, scanActivityRow, "scan activity row", "iterate recent activities")
}

// ListPublicActivitiesForUser returns a user's public, non-hidden activities
// newest-first — the source for their ActivityPub outbox (federation Phase 3).
func (r *ActivityRepo) ListPublicActivitiesForUser(ctx context.Context, userID domain.UserID, limit, offset int) ([]domain.Activity, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT `+activityColumns+`
		   FROM activities
		  WHERE user_id = $1 AND deleted_at IS NULL
		    AND privacy = 'public' AND hidden_by_admin = false
		  ORDER BY start_time DESC
		  LIMIT $2 OFFSET $3`,
		userID.UUID(), limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list public activities for %s: %w", userID, err)
	}
	defer rows.Close()
	var out []domain.Activity
	for rows.Next() {
		a, err := scanActivityRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan activity row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountPublicActivitiesForUser counts a user's public, non-hidden activities
// (the outbox totalItems).
func (r *ActivityRepo) CountPublicActivitiesForUser(ctx context.Context, userID domain.UserID) (int, error) {
	db := dbtx(ctx, r.pool)
	var n int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM activities
		  WHERE user_id = $1 AND deleted_at IS NULL
		    AND privacy = 'public' AND hidden_by_admin = false`,
		userID.UUID()).Scan(&n); err != nil {
		return 0, fmt.Errorf("count public activities for %s: %w", userID, err)
	}
	return n, nil
}

// ActivityTotals returns aggregate sums across the user's non-deleted activities.
func (r *ActivityRepo) ActivityTotals(ctx context.Context, userID domain.UserID) (port.ActivityTotalsResult, error) {
	db := dbtx(ctx, r.pool)
	var out port.ActivityTotalsResult
	err := db.QueryRow(ctx,
		`SELECT count(*),
		        COALESCE(sum(distance_m), 0),
		        COALESCE(sum(moving_duration_s), 0),
		        COALESCE(sum(elevation_gain_m), 0)
		   FROM activities
		  WHERE user_id = $1 AND deleted_at IS NULL`,
		userID.UUID()).Scan(&out.Count, &out.DistanceM, &out.MovingS, &out.ElevationGainM)
	if err != nil {
		return port.ActivityTotalsResult{}, fmt.Errorf("activity totals: %w", err)
	}
	return out, nil
}

// ActivityYearStats returns per-calendar-year totals (newest first).
func (r *ActivityRepo) ActivityYearStats(ctx context.Context, userID domain.UserID) ([]port.ActivityYearStat, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT EXTRACT(YEAR FROM start_time)::int AS yr,
		        count(*),
		        COALESCE(sum(distance_m), 0),
		        COALESCE(sum(moving_duration_s), 0),
		        COALESCE(sum(elevation_gain_m), 0)
		   FROM activities
		  WHERE user_id = $1 AND deleted_at IS NULL
		  GROUP BY yr ORDER BY yr DESC`,
		userID.UUID())
	if err != nil {
		return nil, fmt.Errorf("activity year stats: %w", err)
	}
	defer rows.Close()
	var out []port.ActivityYearStat
	for rows.Next() {
		var s port.ActivityYearStat
		if err := rows.Scan(&s.Year, &s.Count, &s.DistanceM, &s.MovingS, &s.ElevationGainM); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// activityFilterConds translates an ActivityListFilter into WHERE conditions
// and their args, starting from the user + not-deleted base. excludeDim names
// a facet dimension ("type" | "discipline") whose own equality filter is
// skipped — a facet must ignore its own dimension so the user can still switch
// within it, while respecting every other active filter.
func activityFilterConds(
	userID domain.UserID,
	filter port.ActivityListFilter,
	excludeDim string,
) (conds []string, args []any) {
	conds = []string{"user_id = $1", "deleted_at IS NULL"}
	args = []any{userID.UUID()}

	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}

	if filter.Type != "" && excludeDim != "type" {
		add("type = $%d", filter.Type)
	}
	if filter.Discipline != "" && excludeDim != "discipline" {
		add("discipline = $%d", filter.Discipline)
	}
	if !filter.From.IsZero() {
		add("start_time >= $%d", filter.From)
	}
	if !filter.To.IsZero() {
		add("start_time < $%d", filter.To)
	}
	if len(filter.GearIDs) > 0 {
		add("gear_id = ANY($%d)", gearIDParams(filter.GearIDs))
	}

	for _, f := range []struct {
		col string
		val *bool
	}{
		{"is_virtual", filter.IsVirtual},
		{"is_ebike", filter.IsEbike},
		{"is_commute", filter.IsCommute},
		{"is_race", filter.IsRace},
	} {
		if f.val != nil {
			add(f.col+" = $%d", *f.val)
		}
	}
	activityFilterRanges(filter, add)

	return conds, args
}

// activityFilterRanges adds the bounded numeric ranges. A NULL column value
// never matches (SQL comparison with NULL is not true), which is the intended
// "unknown doesn't match" rule.
func activityFilterRanges(filter port.ActivityListFilter, add func(cond string, val any)) {
	for _, rng := range []struct {
		col      string
		min, max *float64
	}{
		{"distance_m", filter.DistanceMinM, filter.DistanceMaxM},
		{"elapsed_duration_s", filter.DurationMinS, filter.DurationMaxS},
		{"elevation_gain_m", filter.ElevationMinM, filter.ElevationMaxM},
		{"avg_speed_mps", filter.AvgSpeedMinMps, filter.AvgSpeedMaxMps},
		{"avg_heart_rate_bpm", filter.AvgHRMinBpm, filter.AvgHRMaxBpm},
		{"avg_power_w", filter.AvgPowerMinW, filter.AvgPowerMaxW},
	} {
		if rng.min != nil {
			add(rng.col+" >= $%d", *rng.min)
		}
		if rng.max != nil {
			add(rng.col+" <= $%d", *rng.max)
		}
	}
}

// ActivityFacets returns per-type and per-discipline counts (only values the
// user has) plus the grand total. Empty discipline values are skipped in the
// discipline facet (they mean "no sub-classification").
func (r *ActivityRepo) ActivityFacets(
	ctx context.Context,
	userID domain.UserID,
	filter port.ActivityListFilter,
) (types, disciplines []port.ActivityFacet, total int, err error) {
	db := dbtx(ctx, r.pool)

	// facet counts distinct values of `col`, respecting every active filter
	// except the facet's own dimension.
	facet := func(col string, skipEmpty bool) ([]port.ActivityFacet, error) {
		conds, args := activityFilterConds(userID, filter, col)
		if skipEmpty {
			conds = append(conds, col+" <> ''")
		}
		rows, qerr := db.Query(ctx,
			`SELECT `+col+`, count(*) FROM activities WHERE `+strings.Join(conds, " AND ")+
				` GROUP BY `+col+` ORDER BY count(*) DESC`, args...)
		if qerr != nil {
			return nil, qerr
		}
		defer rows.Close()
		var out []port.ActivityFacet
		for rows.Next() {
			var f port.ActivityFacet
			if e := rows.Scan(&f.Value, &f.Count); e != nil {
				return nil, e
			}
			out = append(out, f)
		}
		return out, rows.Err()
	}

	if types, err = facet("type", false); err != nil {
		return nil, nil, 0, fmt.Errorf("facet type: %w", err)
	}
	if disciplines, err = facet("discipline", true); err != nil {
		return nil, nil, 0, fmt.Errorf("facet discipline: %w", err)
	}
	if err = db.QueryRow(ctx,
		`SELECT count(*) FROM activities WHERE user_id = $1 AND deleted_at IS NULL`,
		userID.UUID()).Scan(&total); err != nil {
		return nil, nil, 0, fmt.Errorf("facet total: %w", err)
	}
	return types, disciplines, total, nil
}

// activitySortSQL maps the filter sort key to an ORDER BY clause. Unknown keys
// fall back to newest-first.
func activitySortSQL(sort string) string {
	switch sort {
	case "date_asc":
		return "start_time ASC"
	case "distance_desc":
		return "distance_m DESC NULLS LAST"
	case "duration_desc":
		return "elapsed_duration_s DESC NULLS LAST"
	case "elevation_desc":
		return "elevation_gain_m DESC NULLS LAST"
	case "speed_desc":
		return "avg_speed_mps DESC NULLS LAST"
	default: // "date_desc"
		return "start_time DESC"
	}
}

// ListActivitiesFiltered returns a filtered+sorted page plus the total count
// matching the filter (ignoring limit).
func (r *ActivityRepo) ListActivitiesFiltered(
	ctx context.Context,
	userID domain.UserID,
	filter port.ActivityListFilter,
) ([]domain.Activity, int, error) {
	db := dbtx(ctx, r.pool)

	conds, args := activityFilterConds(userID, filter, "")
	where := strings.Join(conds, " AND ")

	var matched int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM activities WHERE `+where, args...).Scan(&matched); err != nil {
		return nil, 0, fmt.Errorf("count filtered activities: %w", err)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit)
	limitIdx := len(args)
	args = append(args, offset)
	offsetIdx := len(args)
	rows, err := db.Query(ctx,
		`SELECT `+activityColumns+`
		   FROM activities WHERE `+where+
			// id tiebreak keeps ordering stable across pages when sort keys tie.
			` ORDER BY `+activitySortSQL(filter.Sort)+`, id DESC`+
			fmt.Sprintf(` LIMIT $%d OFFSET $%d`, limitIdx, offsetIdx),
		args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list filtered activities: %w", err)
	}
	defer rows.Close()

	var out []domain.Activity
	for rows.Next() {
		a, err := scanActivityRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan activity row: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate filtered activities: %w", err)
	}
	return out, matched, nil
}

// ListFollowingFeed returns non-private activities owned by users the viewer
// follows (accepted edges), newest first. The privacy filter is coarse here
// (private excluded); per-category projection happens at the read path.
func (r *ActivityRepo) ListFollowingFeed(
	ctx context.Context,
	viewerID domain.UserID,
	limit, offset int,
) ([]domain.Activity, error) {
	if limit <= 0 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT `+activityColumns+`
		   FROM activities a
		   WHERE a.deleted_at IS NULL
		     AND a.privacy <> 'private'
		     AND a.hidden_by_admin = false
		     AND a.user_id IN (
		         SELECT followee_id FROM follows
		         WHERE follower_id = $1 AND status = 'accepted'
		     )
		   ORDER BY a.start_time DESC, a.id DESC
		   LIMIT $2 OFFSET $3`,
		viewerID.UUID(), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list following feed: %w", err)
	}
	defer rows.Close()
	var out []domain.Activity
	for rows.Next() {
		a, err := scanActivityRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan feed row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// scan helpers
// ---------------------------------------------------------------------------

// rowScanner is the common scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanActivityRow(row rowScanner) (domain.Activity, error) {
	var (
		a  domain.Activity
		x  activityScanExtras
		sm = &a.Summary
	)
	err := row.Scan(
		&x.id, &x.userID,
		&x.typ, &x.discipline, &a.IsVirtual, &a.IsEbike, &a.IsCommute, &a.IsRace, &a.CustomSubtype,
		&a.Title, &a.Description,
		&a.StartTime, &a.EndTime, &x.elapsedS, &x.movingS, &a.Timezone,
		&sm.DistanceM, &sm.ElevationGainM, &sm.ElevationLossM, &sm.MinElevationM, &sm.MaxElevationM,
		&sm.AvgSpeedMps, &sm.MaxSpeedMps,
		&sm.AvgHeartRateBpm, &sm.MaxHeartRateBpm,
		&sm.AvgPowerW, &sm.MaxPowerW, &sm.NormalizedPowerW,
		&sm.AvgCadence, &sm.MaxCadence,
		&sm.AvgTemperatureC, &sm.MinTemperatureC, &sm.MaxTemperatureC,
		&sm.CaloriesKcal, &sm.TSS, &sm.IntensityFactor,
		&sm.PoolLengthM, &sm.TotalStrokes,
		&x.provenanceRaw,
		&x.primaryStreamSourceID,
		&a.MergedAt,
		&x.gearID, &a.Tags, &x.privacy,
		&a.CreatedAt, &a.UpdatedAt, &a.DeletedAt,
		&a.StartLat, &a.StartLng, &x.startPlace,
		&a.HiddenByAdmin,
		&x.gearSetBy,
	)
	if err != nil {
		return domain.Activity{}, err
	}
	if err := x.apply(&a); err != nil {
		return domain.Activity{}, err
	}
	return a, nil
}

// activityScanExtras holds activity columns that need conversion after Scan.
type activityScanExtras struct {
	id, userID            uuid.UUID
	typ                   string
	discipline            *string
	elapsedS, movingS     int64
	provenanceRaw         []byte
	primaryStreamSourceID *uuid.UUID
	gearID                *uuid.UUID
	gearSetBy             string
	privacy               string
	startPlace            *string
}

// gearSetByOrMerge maps the zero value to the column default.
func gearSetByOrMerge(s domain.GearSetBy) domain.GearSetBy {
	if s == "" {
		return domain.GearSetByMerge
	}
	return s
}

// apply converts the scanned extras onto a.
func (x *activityScanExtras) apply(a *domain.Activity) error {
	prov, err := decodeProvenance(x.provenanceRaw)
	if err != nil {
		return err
	}
	a.ID = domain.ActivityID(x.id)
	a.UserID = domain.UserID(x.userID)
	a.Type = domain.ActivityType(x.typ)
	a.ElapsedDuration = time.Duration(x.elapsedS) * time.Second
	a.MovingDuration = time.Duration(x.movingS) * time.Second
	a.MergeProvenance = prov
	a.Privacy = domain.ActivityPrivacy(x.privacy)
	if x.startPlace != nil {
		a.StartPlace = *x.startPlace
	}
	if x.discipline != nil {
		a.Discipline = domain.Discipline(*x.discipline)
	}
	if x.primaryStreamSourceID != nil {
		sid := domain.SourceID(*x.primaryStreamSourceID)
		a.PrimaryStreamSourceID = &sid
	}
	if x.gearID != nil {
		gid := domain.GearID(*x.gearID)
		a.GearID = &gid
	}
	a.GearSetBy = gearSetByOrMerge(domain.GearSetBy(x.gearSetBy))
	return nil
}

func scanSourceRow(row rowScanner) (domain.ActivitySource, error) {
	var (
		s                      domain.ActivitySource
		id, activityID, userID uuid.UUID
		externalAccountID      *uuid.UUID
		parsedRaw              []byte
		status, reimport       string
	)

	err := row.Scan(
		&id, &activityID, &userID,
		&s.Provider, &externalAccountID, &s.ExternalID,
		&s.SourceWorkerName, &s.SourceWorkerVersion, &s.SourceWorkerPackage,
		&s.RawBlobID, &s.RawContentType, &s.RawSizeBytes,
		&parsedRaw,
		&status, &s.StatusReason,
		&reimport, &s.ReimportStatusReason,
		&s.ImportedAt, &s.LastReimportedAt, &s.UpdatedAt,
		&s.SourceWorkerRelease,
	)
	if err != nil {
		return domain.ActivitySource{}, err
	}

	parsed, err := decodePayload(parsedRaw)
	if err != nil {
		return domain.ActivitySource{}, err
	}

	s.ID = domain.SourceID(id)
	s.ActivityID = domain.ActivityID(activityID)
	s.UserID = domain.UserID(userID)
	s.Parsed = parsed
	s.Status = domain.SourceStatus(status)
	s.ReimportStatus = domain.ReimportStatus(reimport)
	if externalAccountID != nil {
		eaID := domain.ExternalAccountID(*externalAccountID)
		s.ExternalAccountID = &eaID
	}
	return s, nil
}

// nullableTime returns the time when non-zero, else nil. Used for
// COALESCE-driven default-to-now() semantics on insert.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// ActivityTotalsForRange returns totals for activities whose start_time falls in
// [start, end). Used for "this week" / "last week" dashboard stats.
func (r *ActivityRepo) ActivityTotalsForRange(ctx context.Context, userID domain.UserID, start, end time.Time) (port.ActivityTotalsResult, error) {
	db := dbtx(ctx, r.pool)
	var out port.ActivityTotalsResult
	err := db.QueryRow(ctx,
		`SELECT count(*),
		        COALESCE(sum(distance_m), 0),
		        COALESCE(sum(moving_duration_s), 0),
		        COALESCE(sum(elevation_gain_m), 0)
		   FROM activities
		  WHERE user_id = $1 AND deleted_at IS NULL
		    AND start_time >= $2 AND start_time < $3`,
		userID.UUID(), start.UTC(), end.UTC()).Scan(&out.Count, &out.DistanceM, &out.MovingS, &out.ElevationGainM)
	if err != nil {
		return port.ActivityTotalsResult{}, fmt.Errorf("activity totals for range: %w", err)
	}
	return out, nil
}

// SourceWorker is one (pool, package) an account's sources came from.
type SourceWorker struct{ Name, Package string }

// ListSourceWorkersForAccount lists the distinct pools that imported the
// account's live sources.
func (r *ActivityRepo) ListSourceWorkersForAccount(ctx context.Context, acct domain.ExternalAccountID) ([]SourceWorker, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx, `SELECT DISTINCT source_worker_name, source_worker_package FROM activity_sources
		 WHERE external_account_id = $1 AND status != 'detached'`, acct.UUID())
	if err != nil {
		return nil, fmt.Errorf("list source workers: %w", err)
	}
	defer rows.Close()
	var out []SourceWorker
	for rows.Next() {
		var w SourceWorker
		if err := rows.Scan(&w.Name, &w.Package); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CountSourcesOutOfDate counts the account's update_available sources per reason.
func (r *ActivityRepo) CountSourcesOutOfDate(ctx context.Context, acct domain.ExternalAccountID) (map[string]int, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx, `SELECT reimport_status_reason, count(*) FROM activity_sources
		 WHERE external_account_id = $1 AND status != 'detached' AND reimport_status = 'update_available'
		 GROUP BY reimport_status_reason`, acct.UUID())
	if err != nil {
		return nil, fmt.Errorf("count sources out of date: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return nil, err
		}
		out[reason] = n
	}
	return out, rows.Err()
}

// ListSourcesOutOfDate lists the account's sources flagged update_available
// for reason, oldest import first, at most limit.
func (r *ActivityRepo) ListSourcesOutOfDate(ctx context.Context, acct domain.ExternalAccountID, reason string, limit int) ([]domain.ActivitySource, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx, `SELECT `+sourceColumns+` FROM activity_sources
		 WHERE external_account_id = $1 AND status != 'detached'
		   AND reimport_status = 'update_available' AND reimport_status_reason = $2
		 ORDER BY imported_at LIMIT $3`, acct.UUID(), reason, limit)
	if err != nil {
		return nil, fmt.Errorf("list sources out of date: %w", err)
	}
	return pgCollectRows(rows, scanSourceRow, "scan source row", "iterate source rows")
}
