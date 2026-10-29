package postgres

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// BestEffortRepo implements port.BestEffortRepo.
type BestEffortRepo struct {
	pool *pgxpool.Pool
}

// ListPersonalRecords returns the user's all-time best per (activity type,
// metric, window) across every activity. DISTINCT ON keeps only the best row
// per group; the ordering makes "best" pace ASC (smaller is better) and every
// other metric DESC (larger is better) via a sign flip. Always current — it
// reads live best_efforts, so a new best automatically becomes the PR with no
// invalidation step.
func (r *BestEffortRepo) ListPersonalRecords(ctx context.Context, userID domain.UserID) ([]domain.PersonalRecord, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx, `
		SELECT DISTINCT ON (activity_type, metric, window_kind, window_value)
		       activity_type, metric, window_kind, window_value,
		       achieved_value, activity_id, ts
		  FROM best_efforts
		 WHERE user_id = $1
		 ORDER BY activity_type, metric, window_kind, window_value,
		          (CASE WHEN metric = 'pace' THEN achieved_value ELSE -achieved_value END) ASC,
		          ts ASC`,
		userID.UUID(),
	)
	if err != nil {
		return nil, fmt.Errorf("list personal records: %w", err)
	}
	defer rows.Close()
	var out []domain.PersonalRecord
	for rows.Next() {
		var (
			at, metric, wk string
			wv             int
			av             float64
			aid            uuid.UUID
			ts             time.Time
		)
		if err := rows.Scan(&at, &metric, &wk, &wv, &av, &aid, &ts); err != nil {
			return nil, fmt.Errorf("scan personal record: %w", err)
		}
		out = append(out, domain.PersonalRecord{
			ActivityType:  domain.ActivityType(at),
			Metric:        domain.BestEffortMetric(metric),
			WindowKind:    domain.BestEffortWindowKind(wk),
			WindowValue:   wv,
			AchievedValue: av,
			ActivityID:    domain.ActivityID(aid),
			Timestamp:     ts,
		})
	}
	return out, rows.Err()
}

func NewBestEffortRepo(pool *pgxpool.Pool) *BestEffortRepo {
	return &BestEffortRepo{pool: pool}
}

const bestEffortColumns = `
	id, activity_id, activity_source_id, user_id,
	activity_type, discipline,
	metric, window_kind, window_value,
	achieved_value,
	start_offset, duration_s, distance_m,
	ts, created_at
`

// DeleteForSource removes all best_efforts rows for the source.
func (r *BestEffortRepo) DeleteForUserWindow(ctx context.Context, userID domain.UserID, activityType domain.ActivityType, metric domain.BestEffortMetric, kind domain.BestEffortWindowKind, value int) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`DELETE FROM best_efforts
		  WHERE user_id = $1 AND activity_type = $2 AND metric = $3 AND window_kind = $4 AND window_value = $5`,
		userID.UUID(), string(activityType), string(metric), string(kind), value)
	if err != nil {
		return fmt.Errorf("delete best_efforts window: %w", err)
	}
	return nil
}

func (r *BestEffortRepo) DeleteForSource(ctx context.Context, sourceID domain.SourceID) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`DELETE FROM best_efforts WHERE activity_source_id = $1`,
		sourceID.UUID(),
	)
	if err != nil {
		return fmt.Errorf("delete best_efforts for %s: %w", sourceID, err)
	}
	return nil
}

// SaveEfforts inserts a batch of best_efforts in one pgx.Batch round trip.
func (r *BestEffortRepo) SaveEfforts(ctx context.Context, efforts []domain.BestEffort) error {
	if len(efforts) == 0 {
		return nil
	}
	db := dbtx(ctx, r.pool)

	batch := &pgx.Batch{}
	for _, e := range efforts {
		var disciplineParam any
		if e.Discipline != "" && e.Discipline != domain.DisciplineNone {
			disciplineParam = string(e.Discipline)
		}
		var createdAtParam any
		if !e.CreatedAt.IsZero() {
			createdAtParam = e.CreatedAt
		}

		batch.Queue(
			`INSERT INTO best_efforts (
				id, activity_id, activity_source_id, user_id,
				activity_type, discipline,
				metric, window_kind, window_value,
				achieved_value,
				start_offset, duration_s, distance_m,
				ts, created_at
			) VALUES (
				$1, $2, $3, $4,
				$5, $6,
				$7, $8, $9,
				$10,
				$11, $12, $13,
				$14, COALESCE($15, now())
			)`,
			e.ID.UUID(), e.ActivityID.UUID(), e.ActivitySourceID.UUID(), e.UserID.UUID(),
			string(e.ActivityType), disciplineParam,
			string(e.Metric), string(e.WindowKind), e.WindowValue,
			e.AchievedValue,
			e.StartOffset, e.DurationS, e.DistanceM,
			e.Timestamp, createdAtParam,
		)
	}

	br := db.(pgxBatchExecer).SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()
	for i := 0; i < len(efforts); i++ {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("save best_effort %d: %w", i, err)
		}
	}
	return nil
}

// ListForActivity returns every best_effort for the activity, ordered
// metric → window_value so the UI's power-curve / pace-curve renderer
// gets adjacent windows next to each other.
func (r *BestEffortRepo) ListForActivity(ctx context.Context, activityID domain.ActivityID) ([]domain.BestEffort, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT `+bestEffortColumns+`
		   FROM best_efforts
		  WHERE activity_id = $1
		  ORDER BY metric, window_kind, window_value`,
		activityID.UUID(),
	)
	if err != nil {
		return nil, fmt.Errorf("list best_efforts for %s: %w", activityID, err)
	}
	return pgCollectRows(rows, scanBestEffortRow, "scan best_effort row", "iterate best_effort rows")
}

// pgxBatchExecer is the subset of *pgxpool.Pool and pgx.Tx needed for
// SendBatch. Used as a local interface so the DBTX surface stays small.
type pgxBatchExecer interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

func scanBestEffortRow(row rowScanner) (domain.BestEffort, error) {
	var (
		id, activityID, sourceID, userID uuid.UUID
		activityType                     string
		discipline                       *string
		metric, windowKind               string
		windowValue                      int
		achievedValue                    float64
		startOffset                      int
		durationS                        float64
		distanceM                        *float64
		ts, createdAt                    time.Time
	)
	if err := row.Scan(
		&id, &activityID, &sourceID, &userID,
		&activityType, &discipline,
		&metric, &windowKind, &windowValue,
		&achievedValue,
		&startOffset, &durationS, &distanceM,
		&ts, &createdAt,
	); err != nil {
		return domain.BestEffort{}, err
	}

	be := domain.BestEffort{
		ID:               domain.BestEffortID(id),
		ActivityID:       domain.ActivityID(activityID),
		ActivitySourceID: domain.SourceID(sourceID),
		UserID:           domain.UserID(userID),
		ActivityType:     domain.ActivityType(activityType),
		Metric:           domain.BestEffortMetric(metric),
		WindowKind:       domain.BestEffortWindowKind(windowKind),
		WindowValue:      windowValue,
		AchievedValue:    achievedValue,
		StartOffset:      startOffset,
		DurationS:        durationS,
		DistanceM:        distanceM,
		Timestamp:        ts,
		CreatedAt:        createdAt,
	}
	if discipline != nil {
		be.Discipline = domain.Discipline(*discipline)
	}
	return be, nil
}

// ListBestEffortHistory returns the user's value for one best-effort bucket
// (metric + window_kind + window_value) across every activity — the
// progression series. When an activity has the bucket on multiple sources, the
// better value wins (min for pace, max otherwise). Sorted chronologically.
func (r *BestEffortRepo) ListBestEffortHistory(
	ctx context.Context,
	userID domain.UserID,
	activityType, metric, windowKind string,
	windowValue int,
) ([]domain.BestEffortHistoryItem, error) {
	betterDir := "DESC" // higher achieved_value is better for power/speed/hr/vam
	if metric == "pace" {
		betterDir = "ASC" // pace stores seconds/km — lower is better
	}
	db := dbtx(ctx, r.pool)
	// Scope by activity_type: a "400 m pace" PR for running and cycling are
	// different efforts — mixing them produces a meaningless series.
	rows, err := db.Query(ctx,
		`SELECT DISTINCT ON (b.activity_id)
		        b.activity_id, a.title, a.start_time, b.achieved_value
		   FROM best_efforts b
		   JOIN activities a ON a.id = b.activity_id AND a.deleted_at IS NULL
		  WHERE b.user_id = $1 AND b.activity_type = $2
		    AND b.metric = $3 AND b.window_kind = $4 AND b.window_value = $5
		  ORDER BY b.activity_id, b.achieved_value `+betterDir,
		userID.UUID(), activityType, metric, windowKind, windowValue,
	)
	if err != nil {
		return nil, fmt.Errorf("list best-effort history: %w", err)
	}
	defer rows.Close()

	var out []domain.BestEffortHistoryItem
	for rows.Next() {
		var (
			it domain.BestEffortHistoryItem
			id uuid.UUID
		)
		if err := rows.Scan(&id, &it.Title, &it.StartTime, &it.AchievedValue); err != nil {
			return nil, fmt.Errorf("scan best-effort history: %w", err)
		}
		it.ActivityID = domain.ActivityID(id)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime.Before(out[j].StartTime) })
	return out, nil
}
