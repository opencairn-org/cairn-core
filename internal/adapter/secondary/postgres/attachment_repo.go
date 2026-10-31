package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// AttachmentRepo implements port.AttachmentRepo over activity_attachments.
type AttachmentRepo struct{ pool *pgxpool.Pool }

func NewAttachmentRepo(pool *pgxpool.Pool) *AttachmentRepo {
	return &AttachmentRepo{pool: pool}
}

func (r *AttachmentRepo) ListForActivity(ctx context.Context, activityID domain.ActivityID) ([]domain.Attachment, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT id, activity_id, source_id, user_id, blob_id, external_url,
		        content_type, caption, width, height, position, created_at,
		        thumb_blob_id, medium_blob_id, blurhash
		   FROM activity_attachments
		  WHERE activity_id = $1
		  ORDER BY position, created_at`,
		activityID.UUID(),
	)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	defer rows.Close()
	var out []domain.Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *AttachmentRepo) Get(ctx context.Context, id domain.AttachmentID) (domain.Attachment, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT id, activity_id, source_id, user_id, blob_id, external_url,
		        content_type, caption, width, height, position, created_at,
		        thumb_blob_id, medium_blob_id, blurhash
		   FROM activity_attachments WHERE id = $1`,
		id.UUID(),
	)
	a, err := scanAttachment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Attachment{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Attachment{}, fmt.Errorf("get attachment: %w", err)
	}
	return a, nil
}

func (r *AttachmentRepo) Add(ctx context.Context, a domain.Attachment) error {
	db := dbtx(ctx, r.pool)
	var src any
	if a.SourceID != nil {
		src = a.SourceID.UUID()
	}
	_, err := db.Exec(ctx,
		`INSERT INTO activity_attachments
		   (activity_id, source_id, user_id, blob_id, external_url, content_type, caption, width, height, position,
		    thumb_blob_id, medium_blob_id, blurhash)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		a.ActivityID.UUID(), src, a.UserID.UUID(), a.BlobID, a.ExternalURL,
		a.ContentType, a.Caption, a.Width, a.Height, a.Position,
		a.ThumbBlobID, a.MediumBlobID, a.Blurhash,
	)
	if err != nil {
		return fmt.Errorf("add attachment: %w", err)
	}
	return nil
}

func (r *AttachmentRepo) ReplaceForSource(ctx context.Context, activityID domain.ActivityID, sourceID domain.SourceID, attachments []domain.Attachment) error {
	db := dbtx(ctx, r.pool)
	if _, err := db.Exec(ctx,
		`DELETE FROM activity_attachments WHERE source_id = $1`, sourceID.UUID(),
	); err != nil {
		return fmt.Errorf("clear source attachments: %w", err)
	}
	for i, a := range attachments {
		if _, err := db.Exec(ctx,
			`INSERT INTO activity_attachments
			   (activity_id, source_id, user_id, blob_id, external_url, content_type, caption, width, height, position)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			activityID.UUID(), sourceID.UUID(), a.UserID.UUID(), a.BlobID, a.ExternalURL,
			a.ContentType, a.Caption, a.Width, a.Height, i,
		); err != nil {
			return fmt.Errorf("insert source attachment: %w", err)
		}
	}
	return nil
}

func (r *AttachmentRepo) Delete(ctx context.Context, id domain.AttachmentID) (string, error) {
	db := dbtx(ctx, r.pool)
	var blobID string
	err := db.QueryRow(ctx,
		`DELETE FROM activity_attachments WHERE id = $1 RETURNING blob_id`,
		id.UUID(),
	).Scan(&blobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("delete attachment: %w", err)
	}
	return blobID, nil
}

func (r *AttachmentRepo) SetVariants(ctx context.Context, id domain.AttachmentID, thumbBlobID, mediumBlobID, blurhash string, width, height int) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE activity_attachments
		    SET thumb_blob_id = $2, medium_blob_id = $3, blurhash = $4, width = $5, height = $6
		  WHERE id = $1`,
		id.UUID(), thumbBlobID, mediumBlobID, blurhash, width, height,
	)
	if err != nil {
		return fmt.Errorf("set attachment variants: %w", err)
	}
	return nil
}

func (r *AttachmentRepo) ListWithoutVariants(ctx context.Context, limit int) ([]domain.Attachment, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT id, activity_id, source_id, user_id, blob_id, external_url,
		        content_type, caption, width, height, position, created_at,
		        thumb_blob_id, medium_blob_id, blurhash
		   FROM activity_attachments
		  WHERE thumb_blob_id = '' OR medium_blob_id = '' OR blurhash = ''
		  ORDER BY created_at
		  LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list attachments without variants: %w", err)
	}
	defer rows.Close()
	var out []domain.Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAttachment(row rowScanner) (domain.Attachment, error) {
	var (
		id, aid, uid uuid.UUID
		sid          *uuid.UUID
		a            domain.Attachment
	)
	if err := row.Scan(&id, &aid, &sid, &uid, &a.BlobID, &a.ExternalURL,
		&a.ContentType, &a.Caption, &a.Width, &a.Height, &a.Position, &a.CreatedAt,
		&a.ThumbBlobID, &a.MediumBlobID, &a.Blurhash); err != nil {
		return domain.Attachment{}, err
	}
	a.ID = domain.AttachmentID(id)
	a.ActivityID = domain.ActivityID(aid)
	a.UserID = domain.UserID(uid)
	if sid != nil {
		s := domain.SourceID(*sid)
		a.SourceID = &s
	}
	return a, nil
}
