package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// MaintenanceRepo implements port.MaintenanceRepo with keyset scans over
// activities, activity_sources and users.
type MaintenanceRepo struct {
	pool *pgxpool.Pool
}

var _ port.MaintenanceRepo = (*MaintenanceRepo)(nil)

func NewMaintenanceRepo(pool *pgxpool.Pool) *MaintenanceRepo { return &MaintenanceRepo{pool: pool} }

func uuidsOf(ids []domain.UserID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.UUID())
	}
	return out
}

func (r *MaintenanceRepo) count(ctx context.Context, sql string, args ...any) (int64, error) {
	var n int64
	if err := r.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("maintenance count: %w", err)
	}
	return n, nil
}

// listIDs runs a keyset query whose first column is a uuid and converts it to T.
func listIDs[T ~[16]byte](ctx context.Context, pool *pgxpool.Pool, what, sql string, args ...any) ([]T, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", what, err)
	}
	defer rows.Close()
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (T, error) {
		var id uuid.UUID
		err := row.Scan(&id)
		return T(id), err
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", what, err)
	}
	return out, nil
}

// userScope restricts col to the $1 user array; an empty array means every user.
func userScope(col string) string {
	return `(cardinality($1::uuid[]) = 0 OR ` + col + ` = ANY($1))`
}

const (
	liveActivities = `activities WHERE deleted_at IS NULL AND `
	liveSources    = `activity_sources s JOIN activities a ON a.id = s.activity_id WHERE a.deleted_at IS NULL AND s.status <> 'detached' AND `
	activeUsers    = `users WHERE status = 'active' AND `
)

func (r *MaintenanceRepo) CountActivities(ctx context.Context, users []domain.UserID) (int64, error) {
	return r.count(ctx, `SELECT count(*) FROM `+liveActivities+userScope("user_id"), uuidsOf(users))
}

func (r *MaintenanceRepo) ListActivityIDs(ctx context.Context, users []domain.UserID, after domain.ActivityID, limit int) ([]domain.ActivityID, error) {
	return listIDs[domain.ActivityID](ctx, r.pool, "activity ids",
		`SELECT id FROM `+liveActivities+userScope("user_id")+` AND id > $2 ORDER BY id LIMIT $3`, uuidsOf(users), after.UUID(), limit)
}

func (r *MaintenanceRepo) CountSources(ctx context.Context, users []domain.UserID) (int64, error) {
	return r.count(ctx, `SELECT count(*) FROM `+liveSources+userScope("s.user_id"), uuidsOf(users))
}

func (r *MaintenanceRepo) ListSourceIDs(ctx context.Context, users []domain.UserID, after domain.SourceID, limit int) ([]domain.SourceID, error) {
	return listIDs[domain.SourceID](ctx, r.pool, "source ids",
		`SELECT s.id FROM `+liveSources+userScope("s.user_id")+` AND s.id > $2 ORDER BY s.id LIMIT $3`, uuidsOf(users), after.UUID(), limit)
}

func (r *MaintenanceRepo) CountUsers(ctx context.Context, users []domain.UserID) (int64, error) {
	return r.count(ctx, `SELECT count(*) FROM `+activeUsers+userScope("id"), uuidsOf(users))
}

func (r *MaintenanceRepo) ListUserIDs(ctx context.Context, users []domain.UserID, after domain.UserID, limit int) ([]domain.UserID, error) {
	return listIDs[domain.UserID](ctx, r.pool, "user ids",
		`SELECT id FROM `+activeUsers+userScope("id")+` AND id > $2 ORDER BY id LIMIT $3`, uuidsOf(users), after.UUID(), limit)
}

func (r *MaintenanceRepo) CountSoftDeletedActivities(ctx context.Context, users []domain.UserID, before time.Time) (int64, error) {
	return r.count(ctx, `SELECT count(*) FROM activities WHERE deleted_at < $2 AND `+userScope("user_id"), uuidsOf(users), before)
}

func (r *MaintenanceRepo) ListSoftDeletedActivityIDs(ctx context.Context, users []domain.UserID, before time.Time, after domain.ActivityID, limit int) ([]domain.ActivityID, error) {
	return listIDs[domain.ActivityID](ctx, r.pool, "soft-deleted ids",
		`SELECT id FROM activities WHERE deleted_at < $2 AND `+userScope("user_id")+` AND id > $3 ORDER BY id LIMIT $4`,
		uuidsOf(users), before, after.UUID(), limit)
}

// HardDeleteActivity collects the blob keys of the activity's sources and
// attachments, deletes the row (dependents cascade) and returns the keys.
// A live (not soft-deleted) activity is left untouched.
func (r *MaintenanceRepo) HardDeleteActivity(ctx context.Context, id domain.ActivityID) ([]string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT raw_blob_id FROM activity_sources WHERE activity_id = $1 AND raw_blob_id <> ''
		UNION SELECT blob_id FROM activity_attachments WHERE activity_id = $1 AND blob_id <> ''
		UNION SELECT thumb_blob_id FROM activity_attachments WHERE activity_id = $1 AND thumb_blob_id <> ''
		UNION SELECT medium_blob_id FROM activity_attachments WHERE activity_id = $1 AND medium_blob_id <> ''`, id.UUID())
	if err != nil {
		return nil, fmt.Errorf("collect blob keys: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("scan blob keys: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM activities WHERE id = $1 AND deleted_at IS NOT NULL`, id.UUID())
	if err != nil {
		return nil, fmt.Errorf("hard delete activity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return keys, nil
}

func (r *MaintenanceRepo) ReferencedBlobKeys(ctx context.Context, keys []string) (map[string]bool, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT k FROM unnest($1::text[]) AS k
		WHERE EXISTS (SELECT 1 FROM activity_sources WHERE raw_blob_id = k)
		   OR EXISTS (SELECT 1 FROM activity_attachments WHERE blob_id = k OR thumb_blob_id = k OR medium_blob_id = k)`, keys)
	if err != nil {
		return nil, fmt.Errorf("referenced blob keys: %w", err)
	}
	defer rows.Close()
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("scan referenced keys: %w", err)
	}
	out := make(map[string]bool, len(found))
	for _, k := range found {
		out[k] = true
	}
	return out, nil
}
