package postgres

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ListSourcesForAccount returns the account's live sources, oldest first.
func (r *ActivityRepo) ListSourcesForAccount(ctx context.Context, acct domain.ExternalAccountID) ([]domain.ActivitySource, error) {
	rows, err := dbtx(ctx, r.pool).Query(ctx,
		`SELECT `+sourceColumns+` FROM activity_sources WHERE external_account_id = $1 AND status != 'detached' ORDER BY imported_at`, acct.UUID())
	if err != nil {
		return nil, fmt.Errorf("list sources for account %s: %w", acct, err)
	}
	return pgCollectRows(rows, scanSourceRow, "scan source row", "iterate source rows")
}

// DeleteAccount drops the row; the schema cascades the queue, import
// events, gear and mirrored segments and nulls the source/metric links.
func (r *ExternalAccountRepo) DeleteAccount(ctx context.Context, id domain.ExternalAccountID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM external_accounts WHERE id = $1`, id.UUID())
	if err != nil {
		return fmt.Errorf("delete external account %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
