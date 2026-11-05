package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ProviderRepo stores worker-declared provider meta (port.ProviderRegistry).
type ProviderRepo struct{ pool *pgxpool.Pool }

func NewProviderRepo(pool *pgxpool.Pool) *ProviderRepo { return &ProviderRepo{pool: pool} }

func (r *ProviderRepo) Upsert(ctx context.Context, meta domain.ProviderMeta, pkg string) error {
	if pkg == "" {
		return errors.New("upsert provider: package required")
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode provider meta: %w", err)
	}
	db := dbtx(ctx, r.pool)
	var bound string
	// A seeded row (package NULL) takes the first publisher; a bound row only
	// accepts its own package.
	err = db.QueryRow(ctx, `
		INSERT INTO providers (provider, package, meta)
		VALUES ($1, $2, $3)
		ON CONFLICT (provider) DO UPDATE SET
			meta       = CASE WHEN providers.package IS NULL OR providers.package = EXCLUDED.package THEN EXCLUDED.meta ELSE providers.meta END,
			updated_at = CASE WHEN providers.package IS NULL OR providers.package = EXCLUDED.package THEN now() ELSE providers.updated_at END,
			package    = COALESCE(providers.package, EXCLUDED.package)
		RETURNING package`, meta.ID, pkg, body).Scan(&bound)
	if err != nil {
		return fmt.Errorf("upsert provider %s: %w", meta.ID, err)
	}
	if bound != pkg {
		return fmt.Errorf("provider %s: %w (%s, reported %s)", meta.ID, domain.ErrProviderPackageChanged, bound, pkg)
	}
	return nil
}

func (r *ProviderRepo) Get(ctx context.Context, id string) (domain.ProviderMeta, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx, `SELECT provider, package, meta, updated_at FROM providers WHERE provider = $1`, id)
	if err != nil {
		return domain.ProviderMeta{}, fmt.Errorf("get provider: %w", err)
	}
	defer rows.Close()
	out, err := scanProviders(rows)
	if err != nil {
		return domain.ProviderMeta{}, err
	}
	if len(out) == 0 {
		return domain.ProviderMeta{}, fmt.Errorf("provider %s: %w", id, domain.ErrNotFound)
	}
	return out[0], nil
}

func (r *ProviderRepo) List(ctx context.Context) ([]domain.ProviderMeta, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx, `SELECT provider, package, meta, updated_at FROM providers ORDER BY provider`)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()
	return scanProviders(rows)
}

func scanProviders(rows pgx.Rows) ([]domain.ProviderMeta, error) {
	var out []domain.ProviderMeta
	for rows.Next() {
		var (
			m    domain.ProviderMeta
			pkg  *string
			body []byte
		)
		if err := rows.Scan(&m.ID, &pkg, &body, &m.UpdatedAt); err != nil {
			return nil, err
		}
		id, at := m.ID, m.UpdatedAt
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("decode provider %s meta: %w", id, err)
		}
		m.ID, m.UpdatedAt = id, at
		if pkg != nil {
			m.Package = *pkg
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *ProviderRepo) Delete(ctx context.Context, id string) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx, `DELETE FROM providers WHERE provider = $1`, id)
	if err != nil {
		return fmt.Errorf("delete provider: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("provider %s: %w", id, domain.ErrNotFound)
	}
	return nil
}
