package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// CustomBestEffortRepo implements port.CustomBestEffortRepo over custom_best_efforts.
type CustomBestEffortRepo struct {
	pool *pgxpool.Pool
}

var _ port.CustomBestEffortRepo = (*CustomBestEffortRepo)(nil)

func NewCustomBestEffortRepo(pool *pgxpool.Pool) *CustomBestEffortRepo {
	return &CustomBestEffortRepo{pool: pool}
}

func (r *CustomBestEffortRepo) Create(ctx context.Context, c domain.CustomBestEffort) (domain.CustomBestEffortID, error) {
	db := dbtx(ctx, r.pool)
	var id uuid.UUID
	err := db.QueryRow(ctx,
		`INSERT INTO custom_best_efforts (user_id, activity_type, metric, window_kind, window_value, label)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		c.UserID.UUID(), string(c.ActivityType), string(c.Metric), string(c.WindowKind), c.WindowValue, c.Label,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.CustomBestEffortID{}, domain.ErrCustomEffortDuplicate
		}
		return domain.CustomBestEffortID{}, fmt.Errorf("insert custom best effort: %w", err)
	}
	return domain.CustomBestEffortID(id), nil
}

func (r *CustomBestEffortRepo) ListForUser(ctx context.Context, userID domain.UserID, activityType domain.ActivityType) ([]domain.CustomBestEffort, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT id, user_id, activity_type, metric, window_kind, window_value, label, created_at
		   FROM custom_best_efforts
		  WHERE user_id = $1 AND ($2 = '' OR activity_type = $2)
		  ORDER BY created_at ASC`,
		userID.UUID(), string(activityType))
	if err != nil {
		return nil, fmt.Errorf("list custom best efforts: %w", err)
	}
	defer rows.Close()
	out := []domain.CustomBestEffort{}
	for rows.Next() {
		var (
			id, uid     uuid.UUID
			at, m, wk   string
			c           domain.CustomBestEffort
			createdAt   time.Time
			windowValue int
		)
		if err := rows.Scan(&id, &uid, &at, &m, &wk, &windowValue, &c.Label, &createdAt); err != nil {
			return nil, fmt.Errorf("scan custom best effort: %w", err)
		}
		c.ID, c.UserID = domain.CustomBestEffortID(id), domain.UserID(uid)
		c.ActivityType, c.Metric, c.WindowKind = domain.ActivityType(at), domain.BestEffortMetric(m), domain.BestEffortWindowKind(wk)
		c.WindowValue, c.CreatedAt = windowValue, createdAt
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *CustomBestEffortRepo) Delete(ctx context.Context, userID domain.UserID, id domain.CustomBestEffortID) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx, `DELETE FROM custom_best_efforts WHERE id = $1 AND user_id = $2`, id.UUID(), userID.UUID())
	if err != nil {
		return fmt.Errorf("delete custom best effort: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("custom best effort %s: %w", id, domain.ErrNotFound)
	}
	return nil
}
