package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

type WorkerPoolRepo struct{ pool *pgxpool.Pool }

func NewWorkerPoolRepo(pool *pgxpool.Pool) *WorkerPoolRepo { return &WorkerPoolRepo{pool: pool} }

func (r *WorkerPoolRepo) Upsert(ctx context.Context, p domain.WorkerPool) error {
	db := dbtx(ctx, r.pool)
	var stored string
	err := db.QueryRow(ctx, `
		INSERT INTO worker_pools (provider, name, package)
		VALUES ($1, $2, $3)
		ON CONFLICT (provider, name) DO UPDATE SET
			last_seen_at = now()
		RETURNING package`, p.Provider, p.Name, p.Package).Scan(&stored)
	if err != nil {
		return fmt.Errorf("upsert worker pool %s/%s: %w", p.Provider, p.Name, err)
	}
	if stored != p.Package {
		return fmt.Errorf("worker pool %s/%s: %w (%s, reported %s)", p.Provider, p.Name, domain.ErrPoolPackageChanged, stored, p.Package)
	}
	return nil
}

func (r *WorkerPoolRepo) Get(ctx context.Context, provider, name string) (domain.WorkerPool, error) {
	db := dbtx(ctx, r.pool)
	p := domain.WorkerPool{Provider: provider, Name: name}
	err := db.QueryRow(ctx, `SELECT package, first_seen_at, last_seen_at FROM worker_pools WHERE provider = $1 AND name = $2`,
		provider, name).Scan(&p.Package, &p.FirstSeenAt, &p.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.WorkerPool{}, fmt.Errorf("worker pool %s/%s: %w", provider, name, domain.ErrNotFound)
	}
	if err != nil {
		return domain.WorkerPool{}, fmt.Errorf("get worker pool: %w", err)
	}
	return p, nil
}

func (r *WorkerPoolRepo) ListForProvider(ctx context.Context, provider string) ([]domain.WorkerPool, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx, `SELECT name, package, first_seen_at, last_seen_at FROM worker_pools WHERE provider = $1 ORDER BY name`, provider)
	if err != nil {
		return nil, fmt.Errorf("list worker pools: %w", err)
	}
	defer rows.Close()
	var out []domain.WorkerPool
	for rows.Next() {
		p := domain.WorkerPool{Provider: provider}
		if err := rows.Scan(&p.Name, &p.Package, &p.FirstSeenAt, &p.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
