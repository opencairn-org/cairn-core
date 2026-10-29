package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// SourcePreviewRepo implements port.SourcePreviewRepo over activity_source_previews.
type SourcePreviewRepo struct {
	pool *pgxpool.Pool
}

var _ port.SourcePreviewRepo = (*SourcePreviewRepo)(nil)

func NewSourcePreviewRepo(pool *pgxpool.Pool) *SourcePreviewRepo {
	return &SourcePreviewRepo{pool: pool}
}

const previewColumns = `p.id, p.activity_source_id, s.activity_id, p.user_id, p.job_id, p.status,
	p.parsed, p.source_diff, p.merged_diff, p.created_at, p.expires_at`

func (r *SourcePreviewRepo) Create(ctx context.Context, p domain.SourcePreview) (domain.SourcePreviewID, error) {
	db := dbtx(ctx, r.pool)
	var id uuid.UUID
	err := db.QueryRow(ctx,
		`INSERT INTO activity_source_previews (activity_source_id, user_id, job_id, status, expires_at)
		 VALUES ($1, $2, $3, 'pending', $4) RETURNING id`,
		p.SourceID.UUID(), p.UserID.UUID(), p.JobID, p.ExpiresAt).Scan(&id)
	if err != nil {
		return domain.SourcePreviewID{}, fmt.Errorf("insert source preview: %w", err)
	}
	return domain.SourcePreviewID(id), nil
}

func (r *SourcePreviewRepo) Get(ctx context.Context, id domain.SourcePreviewID) (domain.SourcePreview, error) {
	db := dbtx(ctx, r.pool)
	p, err := scanPreview(db.QueryRow(ctx,
		`SELECT `+previewColumns+` FROM activity_source_previews p
		   JOIN activity_sources s ON s.id = p.activity_source_id
		  WHERE p.id = $1`, id.UUID()))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SourcePreview{}, fmt.Errorf("source preview %s: %w", id, domain.ErrNotFound)
	}
	return p, err
}

func (r *SourcePreviewRepo) FindPendingForSource(ctx context.Context, sourceID domain.SourceID, now time.Time) (domain.SourcePreview, error) {
	db := dbtx(ctx, r.pool)
	p, err := scanPreview(db.QueryRow(ctx,
		`SELECT `+previewColumns+` FROM activity_source_previews p
		   JOIN activity_sources s ON s.id = p.activity_source_id
		  WHERE p.activity_source_id = $1 AND p.status = 'pending' AND p.expires_at > $2
		  ORDER BY p.created_at ASC LIMIT 1`, sourceID.UUID(), now))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SourcePreview{}, fmt.Errorf("pending preview for %s: %w", sourceID, domain.ErrNotFound)
	}
	return p, err
}

func (r *SourcePreviewRepo) MarkReady(ctx context.Context, id domain.SourcePreviewID, parsed []byte, sourceDiff, mergedDiff []domain.FieldChange) error {
	db := dbtx(ctx, r.pool)
	sd, _ := json.Marshal(nonNilChanges(sourceDiff))
	md, _ := json.Marshal(nonNilChanges(mergedDiff))
	if len(parsed) == 0 {
		parsed = []byte("{}")
	}
	tag, err := db.Exec(ctx,
		`UPDATE activity_source_previews
		    SET status = 'ready', parsed = $2, source_diff = $3, merged_diff = $4
		  WHERE id = $1`, id.UUID(), parsed, sd, md)
	if err != nil {
		return fmt.Errorf("mark preview ready: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("source preview %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

func (r *SourcePreviewRepo) Delete(ctx context.Context, userID domain.UserID, id domain.SourcePreviewID) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx, `DELETE FROM activity_source_previews WHERE id = $1 AND user_id = $2`, id.UUID(), userID.UUID())
	if err != nil {
		return fmt.Errorf("delete source preview: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("source preview %s: %w", id, domain.ErrNotFound)
	}
	return nil
}

func (r *SourcePreviewRepo) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx, `DELETE FROM activity_source_previews WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("delete expired previews: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func scanPreview(row pgx.Row) (domain.SourcePreview, error) {
	var (
		id, src, act, uid uuid.UUID
		p                 domain.SourcePreview
		status            string
		sd, md            []byte
	)
	if err := row.Scan(&id, &src, &act, &uid, &p.JobID, &status, &p.Parsed, &sd, &md, &p.CreatedAt, &p.ExpiresAt); err != nil {
		return domain.SourcePreview{}, err
	}
	p.ID, p.SourceID, p.ActivityID, p.UserID = domain.SourcePreviewID(id), domain.SourceID(src), domain.ActivityID(act), domain.UserID(uid)
	p.Status = domain.SourcePreviewStatus(status)
	_ = json.Unmarshal(sd, &p.SourceDiff)
	_ = json.Unmarshal(md, &p.MergedDiff)
	return p, nil
}

func nonNilChanges(c []domain.FieldChange) []domain.FieldChange {
	if c == nil {
		return []domain.FieldChange{}
	}
	return c
}
