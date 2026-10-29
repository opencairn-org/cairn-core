package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ImportQueueRepo implements port.ImportQueueRepo over the import_queue table.
type ImportQueueRepo struct {
	pool *pgxpool.Pool
}

func NewImportQueueRepo(pool *pgxpool.Pool) *ImportQueueRepo {
	return &ImportQueueRepo{pool: pool}
}

const importQueueCols = `
	id, external_account_id, user_id, provider,
	item_type, external_id, item_time,
	status, priority, attempts, last_error,
	created_at, started_at, completed_at`

func (r *ImportQueueRepo) Enqueue(ctx context.Context, items []domain.ImportQueueItem) (int, error) {
	return r.insert(ctx, items, `ON CONFLICT (external_account_id, item_type, external_id) DO NOTHING`)
}

func (r *ImportQueueRepo) Requeue(ctx context.Context, items []domain.ImportQueueItem) (int, error) {
	// A finished row goes back to pending as a fresh attempt; a pending or
	// in_progress row is left alone so overlapping syncs don't reset work.
	return r.insert(ctx, items, `ON CONFLICT (external_account_id, item_type, external_id) DO UPDATE SET
				status = 'pending', attempts = 0, last_error = NULL,
				started_at = NULL, completed_at = NULL,
				item_time = EXCLUDED.item_time, updated_at = now()
			WHERE import_queue.status IN ('done', 'failed', 'skipped')`)
}

// insert bulk-inserts items with the given ON CONFLICT clause and returns
// the number of rows inserted or updated.
func (r *ImportQueueRepo) insert(ctx context.Context, items []domain.ImportQueueItem, onConflict string) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	for _, it := range items {
		batch.Queue(`
			INSERT INTO import_queue
				(external_account_id, user_id, provider, item_type, external_id, item_time)
			VALUES ($1, $2, $3, $4, $5, $6)
			`+onConflict,
			it.ExternalAccountID.UUID(), it.UserID.UUID(), it.Provider,
			string(it.ItemType), it.ExternalID, it.ItemTime)
	}
	br := r.pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()

	affected := 0
	for range items {
		tag, err := br.Exec()
		if err != nil {
			return affected, fmt.Errorf("enqueue: %w", err)
		}
		affected += int(tag.RowsAffected())
	}
	return affected, nil
}

func (r *ImportQueueRepo) ClaimPending(ctx context.Context, accountID domain.ExternalAccountID, limit int) ([]domain.ImportQueueItem, error) {
	// RETURNING alone yields heap order, not the pick order — re-sort so the
	// caller dispatches a batch highest-priority/newest first.
	q := `
		WITH claimed AS (
			UPDATE import_queue SET
				status = 'in_progress', attempts = attempts + 1,
				started_at = now(), updated_at = now()
			WHERE id IN (
				SELECT id FROM import_queue
				WHERE external_account_id = $1 AND status = 'pending'
				ORDER BY priority DESC, item_time DESC NULLS LAST
				LIMIT $2
				FOR UPDATE SKIP LOCKED
			)
			RETURNING ` + importQueueCols + `
		)
		SELECT ` + importQueueCols + `
		FROM claimed
		ORDER BY priority DESC, item_time DESC NULLS LAST`
	rows, err := r.pool.Query(ctx, q, accountID.UUID(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim pending: %w", err)
	}
	defer rows.Close()

	var out []domain.ImportQueueItem
	for rows.Next() {
		it, err := scanQueueItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (r *ImportQueueRepo) MarkDone(ctx context.Context, id domain.ImportQueueItemID) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE import_queue SET status='done', completed_at=now(), updated_at=now(), last_error=NULL WHERE id=$1`,
		id.UUID())
	return err
}

func (r *ImportQueueRepo) MarkDoneByExternalID(ctx context.Context, accountID domain.ExternalAccountID, itemType domain.ImportItemType, externalID string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE import_queue SET status='done', completed_at=now(), updated_at=now(), last_error=NULL
		 WHERE external_account_id=$1 AND item_type=$2 AND external_id=$3 AND status='in_progress'`,
		accountID.UUID(), string(itemType), externalID)
	return err
}

func (r *ImportQueueRepo) MarkFailedByExternalID(ctx context.Context, accountID domain.ExternalAccountID, itemType domain.ImportItemType, externalID, reason string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE import_queue SET status='failed', completed_at=now(), updated_at=now(), last_error=$4
		 WHERE external_account_id=$1 AND item_type=$2 AND external_id=$3 AND status='in_progress'`,
		accountID.UUID(), string(itemType), externalID, reason)
	return err
}

func (r *ImportQueueRepo) MarkFailed(ctx context.Context, id domain.ImportQueueItemID, reason string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE import_queue SET status='failed', completed_at=now(), updated_at=now(), last_error=$2 WHERE id=$1`,
		id.UUID(), reason)
	return err
}

func (r *ImportQueueRepo) CountByStatus(ctx context.Context, accountID domain.ExternalAccountID) (map[domain.ImportItemStatus]int, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT status, count(*) FROM import_queue WHERE external_account_id=$1 GROUP BY status`,
		accountID.UUID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[domain.ImportItemStatus]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[domain.ImportItemStatus(s)] = n
	}
	return out, rows.Err()
}

func (r *ImportQueueRepo) ListForAccount(ctx context.Context, accountID domain.ExternalAccountID, statuses []domain.ImportItemStatus, limit int) ([]domain.ImportQueueItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	filter := make([]string, 0, len(statuses))
	for _, s := range statuses {
		filter = append(filter, string(s))
	}
	q := `
		SELECT ` + importQueueCols + `
		FROM import_queue
		WHERE external_account_id = $1
		  AND (cardinality($2::text[]) = 0 OR status = ANY($2))
		ORDER BY (status = 'in_progress') DESC, priority DESC, item_time DESC NULLS LAST, created_at DESC
		LIMIT $3`
	rows, err := r.pool.Query(ctx, q, accountID.UUID(), filter, limit)
	if err != nil {
		return nil, fmt.Errorf("list queue items: %w", err)
	}
	defer rows.Close()

	var out []domain.ImportQueueItem
	for rows.Next() {
		it, err := scanQueueItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (r *ImportQueueRepo) RequeueFailed(ctx context.Context, accountID domain.ExternalAccountID, id domain.ImportQueueItemID) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE import_queue SET
			status = 'pending', attempts = 0, last_error = NULL,
			started_at = NULL, completed_at = NULL, updated_at = now()
		WHERE id = $1 AND external_account_id = $2 AND status = 'failed'`,
		id.UUID(), accountID.UUID())
	if err != nil {
		return false, fmt.Errorf("requeue failed item: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (r *ImportQueueRepo) MoveToTop(ctx context.Context, accountID domain.ExternalAccountID, id domain.ImportQueueItemID) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE import_queue SET
			priority = (
				SELECT coalesce(max(priority), 0) + 1 FROM import_queue
				WHERE external_account_id = $2 AND status = 'pending'
			),
			updated_at = now()
		WHERE id = $1 AND external_account_id = $2 AND status = 'pending'`,
		id.UUID(), accountID.UUID())
	if err != nil {
		return false, fmt.Errorf("move to top: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (r *ImportQueueRepo) RequeueStale(ctx context.Context, olderThan time.Duration, maxAttempts int) (int, int, error) {
	// Fail exhausted items first so the requeue below can't pick them up.
	failed, err := r.pool.Exec(ctx, `
		UPDATE import_queue SET
			status = 'failed', completed_at = now(), updated_at = now(),
			last_error = 'stale: no result after '||attempts||' dispatch attempts'
		WHERE status = 'in_progress' AND started_at < now() - make_interval(secs => $1) AND attempts >= $2`,
		olderThan.Seconds(), maxAttempts)
	if err != nil {
		return 0, 0, fmt.Errorf("fail stale: %w", err)
	}
	requeued, err := r.pool.Exec(ctx, `
		UPDATE import_queue SET
			status = 'pending', started_at = NULL, updated_at = now(),
			last_error = 'stale: requeued after no result'
		WHERE status = 'in_progress' AND started_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return int(failed.RowsAffected()), 0, fmt.Errorf("requeue stale: %w", err)
	}
	return int(requeued.RowsAffected()), int(failed.RowsAffected()), nil
}

func (r *ImportQueueRepo) AccountsWithPending(ctx context.Context, limit int) ([]domain.ExternalAccountID, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT DISTINCT external_account_id FROM import_queue WHERE status='pending' LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ExternalAccountID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, domain.ExternalAccountID(id))
	}
	return out, rows.Err()
}

func scanQueueItem(row rowScanner) (domain.ImportQueueItem, error) {
	var (
		it               domain.ImportQueueItem
		id, acct, user   uuid.UUID
		itemType, status string
		lastErr          *string
	)
	if err := row.Scan(
		&id, &acct, &user, &it.Provider,
		&itemType, &it.ExternalID, &it.ItemTime,
		&status, &it.Priority, &it.Attempts, &lastErr,
		&it.CreatedAt, &it.StartedAt, &it.CompletedAt,
	); err != nil {
		return domain.ImportQueueItem{}, err
	}
	it.ID = domain.ImportQueueItemID(id)
	it.ExternalAccountID = domain.ExternalAccountID(acct)
	it.UserID = domain.UserID(user)
	it.ItemType = domain.ImportItemType(itemType)
	it.Status = domain.ImportItemStatus(status)
	if lastErr != nil {
		it.LastError = *lastErr
	}
	return it, nil
}

// ---- instance-wide (admin) ----

// importQueueWhere renders the admin filter as a WHERE clause.
func importQueueWhere(f port.ImportQueueFilter) (string, []any) {
	where := []string{"true"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if len(f.Statuses) > 0 {
		add("status = ANY($%d)", stringsOf(f.Statuses))
	}
	if len(f.ItemTypes) > 0 {
		add("item_type = ANY($%d)", stringsOf(f.ItemTypes))
	}
	if len(f.Providers) > 0 {
		add("provider = ANY($%d)", f.Providers)
	}
	if f.UserID != nil {
		add("user_id = $%d", f.UserID.UUID())
	}
	if !f.From.IsZero() {
		add("created_at >= $%d", f.From)
	}
	if !f.To.IsZero() {
		add("created_at < $%d", f.To)
	}
	return strings.Join(where, " AND "), args
}

func (r *ImportQueueRepo) ListAll(ctx context.Context, f port.ImportQueueFilter) ([]domain.ImportQueueItem, int, error) {
	where, args := importQueueWhere(f)
	var total int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM import_queue WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count queue items: %w", err)
	}
	limit, offset := f.Limit, max(0, f.Offset)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit, offset)
	rows, err := r.pool.Query(ctx, `
		SELECT `+importQueueCols+`
		FROM import_queue
		WHERE `+where+`
		ORDER BY (status = 'in_progress') DESC, created_at DESC
		LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list queue items: %w", err)
	}
	defer rows.Close()
	out := []domain.ImportQueueItem{}
	for rows.Next() {
		it, err := scanQueueItem(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

func (r *ImportQueueRepo) Get(ctx context.Context, id domain.ImportQueueItemID) (domain.ImportQueueItem, error) {
	it, err := scanQueueItem(r.pool.QueryRow(ctx, `SELECT `+importQueueCols+` FROM import_queue WHERE id = $1`, id.UUID()))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ImportQueueItem{}, fmt.Errorf("queue item %s: %w", id, domain.ErrNotFound)
	}
	return it, err
}

func (r *ImportQueueRepo) CountAllByStatus(ctx context.Context, f port.ImportQueueFilter) (map[domain.ImportItemStatus]int, error) {
	where, args := importQueueWhere(f)
	rows, err := r.pool.Query(ctx, `SELECT status, count(*) FROM import_queue WHERE `+where+` GROUP BY status`, args...)
	if err != nil {
		return nil, fmt.Errorf("count queue by status: %w", err)
	}
	defer rows.Close()
	out := map[domain.ImportItemStatus]int{}
	for rows.Next() {
		var (
			s string
			n int
		)
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[domain.ImportItemStatus(s)] = n
	}
	return out, rows.Err()
}

func (r *ImportQueueRepo) Retry(ctx context.Context, id domain.ImportQueueItemID, reset bool) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE import_queue SET
			status = 'pending', attempts = CASE WHEN $2 THEN 0 ELSE attempts END, last_error = NULL,
			started_at = NULL, completed_at = NULL, updated_at = now()
		WHERE id = $1 AND status IN ('failed', 'skipped')`, id.UUID(), reset)
	if err != nil {
		return false, fmt.Errorf("retry queue item: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (r *ImportQueueRepo) Cancel(ctx context.Context, id domain.ImportQueueItemID, reason string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE import_queue SET status = 'skipped', last_error = $2, completed_at = now(), updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'in_progress')`, id.UUID(), reason)
	if err != nil {
		return false, fmt.Errorf("cancel queue item: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
