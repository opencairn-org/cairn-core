package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// MaintenanceJobRepo implements port.MaintenanceJobRepo on maintenance_jobs.
type MaintenanceJobRepo struct {
	pool *pgxpool.Pool
}

var _ port.MaintenanceJobRepo = (*MaintenanceJobRepo)(nil)

func NewMaintenanceJobRepo(pool *pgxpool.Pool) *MaintenanceJobRepo {
	return &MaintenanceJobRepo{pool: pool}
}

const maintenanceJobColumns = `id, operation, user_ids, status, estimated, processed, failed, error, triggered_by, queued_at, started_at, finished_at`

func (r *MaintenanceJobRepo) SaveJob(ctx context.Context, job domain.MaintenanceJob) error {
	id, err := uuid.Parse(job.ID)
	if err != nil {
		return fmt.Errorf("maintenance job id: %w", err)
	}
	var triggeredBy *uuid.UUID
	if job.TriggeredBy != nil {
		u := job.TriggeredBy.UUID()
		triggeredBy = &u
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO maintenance_jobs (`+maintenanceJobColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, estimated = EXCLUDED.estimated, processed = EXCLUDED.processed,
			failed = EXCLUDED.failed, error = EXCLUDED.error, started_at = EXCLUDED.started_at, finished_at = EXCLUDED.finished_at, updated_at = now()`,
		id, string(job.Operation), uuidsOf(job.UserIDs), string(job.Status), job.Estimated, job.Processed, job.Failed, job.Error,
		triggeredBy, job.QueuedAt, job.StartedAt, job.FinishedAt)
	if err != nil {
		return fmt.Errorf("save maintenance job: %w", err)
	}
	return nil
}

func (r *MaintenanceJobRepo) GetJob(ctx context.Context, id string) (domain.MaintenanceJob, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return domain.MaintenanceJob{}, domain.ErrNotFound
	}
	rows, err := r.pool.Query(ctx, `SELECT `+maintenanceJobColumns+` FROM maintenance_jobs WHERE id = $1`, uid)
	if err != nil {
		return domain.MaintenanceJob{}, fmt.Errorf("get maintenance job: %w", err)
	}
	job, err := pgx.CollectExactlyOneRow(rows, scanMaintenanceJob)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.MaintenanceJob{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.MaintenanceJob{}, fmt.Errorf("scan maintenance job: %w", err)
	}
	return job, nil
}

func (r *MaintenanceJobRepo) ListJobs(ctx context.Context, limit int) ([]domain.MaintenanceJob, error) {
	return r.list(ctx, `SELECT `+maintenanceJobColumns+` FROM maintenance_jobs ORDER BY queued_at DESC, id DESC LIMIT $1`, limit)
}

func (r *MaintenanceJobRepo) ListOpenJobs(ctx context.Context) ([]domain.MaintenanceJob, error) {
	return r.list(ctx, `SELECT `+maintenanceJobColumns+` FROM maintenance_jobs WHERE status IN ('queued', 'running') ORDER BY queued_at, id`)
}

func (r *MaintenanceJobRepo) list(ctx context.Context, sql string, args ...any) ([]domain.MaintenanceJob, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list maintenance jobs: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanMaintenanceJob)
	if err != nil {
		return nil, fmt.Errorf("scan maintenance jobs: %w", err)
	}
	return out, nil
}

func scanMaintenanceJob(row pgx.CollectableRow) (domain.MaintenanceJob, error) {
	var (
		job         domain.MaintenanceJob
		id          uuid.UUID
		users       []uuid.UUID
		op, status  string
		triggeredBy *uuid.UUID
	)
	if err := row.Scan(&id, &op, &users, &status, &job.Estimated, &job.Processed, &job.Failed, &job.Error, &triggeredBy,
		&job.QueuedAt, &job.StartedAt, &job.FinishedAt); err != nil {
		return job, err
	}
	job.ID, job.Operation, job.Status = id.String(), domain.MaintenanceOperation(op), domain.MaintenanceStatus(status)
	for _, u := range users {
		job.UserIDs = append(job.UserIDs, domain.UserID(u))
	}
	if triggeredBy != nil {
		t := domain.UserID(*triggeredBy)
		job.TriggeredBy = &t
	}
	return job, nil
}
