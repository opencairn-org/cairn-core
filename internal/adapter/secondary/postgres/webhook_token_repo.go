package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

var _ port.WebhookTokenRepo = (*ExternalAccountRepo)(nil)

func (r *ExternalAccountRepo) ListWebhookTokens(ctx context.Context, provider string, limit, offset int) ([]domain.WebhookSigningToken, int, error) {
	if limit <= 0 {
		limit = 50
	}
	var total int
	if err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM external_accounts WHERE ($1 = '' OR provider = $1)`, provider).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count webhook tokens: %w", err)
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, user_id, provider, webhook_signing_token_rotated_at
		FROM external_accounts
		WHERE ($1 = '' OR provider = $1)
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`, provider, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list webhook tokens: %w", err)
	}
	defer rows.Close()
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.WebhookSigningToken, error) {
		var t domain.WebhookSigningToken
		var id, uid uuid.UUID
		err := row.Scan(&id, &uid, &t.Provider, &t.RotatedAt)
		t.ExternalAccountID, t.UserID = domain.ExternalAccountID(id), domain.UserID(uid)
		return t, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("scan webhook tokens: %w", err)
	}
	return out, total, nil
}

func (r *ExternalAccountRepo) SelectAccountsForRotation(ctx context.Context, provider string, rotatedBefore *time.Time, ids []domain.ExternalAccountID) ([]domain.ExternalAccountID, error) {
	raw := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		raw = append(raw, id.UUID())
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id FROM external_accounts
		WHERE ($1 = '' OR provider = $1)
		  AND ($2::timestamptz IS NULL OR webhook_signing_token_rotated_at IS NULL OR webhook_signing_token_rotated_at < $2)
		  AND (cardinality($3::uuid[]) = 0 OR id = ANY($3))
		ORDER BY created_at, id`, provider, rotatedBefore, raw)
	if err != nil {
		return nil, fmt.Errorf("select accounts for rotation: %w", err)
	}
	defer rows.Close()
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.ExternalAccountID, error) {
		var id uuid.UUID
		err := row.Scan(&id)
		return domain.ExternalAccountID(id), err
	})
	if err != nil {
		return nil, fmt.Errorf("scan accounts for rotation: %w", err)
	}
	return out, nil
}

func (r *ExternalAccountRepo) SetWebhookSigningToken(ctx context.Context, id domain.ExternalAccountID, hash []byte, at time.Time) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE external_accounts
		SET webhook_signing_token_hash = $2, webhook_signing_token_rotated_at = $3, updated_at = now()
		WHERE id = $1`, id.UUID(), hash, at)
	if err != nil {
		return fmt.Errorf("set webhook signing token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
