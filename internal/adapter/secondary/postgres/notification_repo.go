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
)

// NotificationRepo implements port.NotificationRepo on top of the
// `notification_events` table.
//
// Coalescing is done as a single CTE per write: a SELECT finds any
// matching event in the last 24h; an UPDATE bumps coalesce_count on
// hit; an INSERT...WHERE NOT EXISTS adds a fresh row otherwise.
// All in one statement so concurrent writes converge correctly.
type NotificationRepo struct {
	pool *pgxpool.Pool
}

func NewNotificationRepo(pool *pgxpool.Pool) *NotificationRepo {
	return &NotificationRepo{pool: pool}
}

const notificationColumns = `
	id, user_id, type, severity,
	title_i18n_key, body_i18n_key, i18n_params,
	activity_id, segment_id, external_account_id, worker_name,
	dedup_key, coalesce_count,
	read, read_at,
	created_at, updated_at
`

// SaveNotifications writes each notification through the coalesce-or-insert
// path. Implemented per-row rather than as a batch because each write needs
// its own coalesce decision; for typical PR dispatch this is a handful of
// rows per ingest.
func (r *NotificationRepo) SaveNotifications(ctx context.Context, notifs []domain.Notification) error {
	if len(notifs) == 0 {
		return nil
	}
	db := dbtx(ctx, r.pool)

	for i := range notifs {
		// Write the PERSISTED id back onto the caller's slice: on a coalesce
		// hit the surviving row's id differs from the in-memory one, and
		// downstream delivery audit needs the real event_id.
		id, err := r.saveOne(ctx, db, notifs[i])
		if err != nil {
			return fmt.Errorf("save notification %d: %w", i, err)
		}
		notifs[i].ID = id
	}
	return nil
}

func (r *NotificationRepo) saveOne(ctx context.Context, db DBTX, n domain.Notification) (domain.NotificationID, error) {
	params, err := n.MarshalI18nParams()
	if err != nil {
		return domain.NotificationID{}, fmt.Errorf("marshal i18n params: %w", err)
	}

	var (
		activityParam any
		segmentParam  any
		extAcctParam  any
	)
	if n.ActivityID != nil {
		activityParam = n.ActivityID.UUID()
	}
	if n.SegmentID != nil {
		segmentParam = n.SegmentID.UUID()
	}
	if n.ExternalAccountID != nil {
		extAcctParam = n.ExternalAccountID.UUID()
	}

	var persisted uuid.UUID
	err = db.QueryRow(ctx, notificationSaveSQL,
		n.ID.UUID(), n.UserID.UUID(), int(n.Type), string(n.Severity),
		n.TitleI18nKey, n.BodyI18nKey, []byte(params),
		activityParam, segmentParam, extAcctParam, n.WorkerName,
		n.DedupKey, n.CoalesceCount,
		n.Read, nullableTime(timeOrZero(n.ReadAt)),
		nullableTime(n.CreatedAt), nullableTime(n.UpdatedAt),
	).Scan(&persisted)
	if err != nil {
		return domain.NotificationID{}, fmt.Errorf("upsert notification %s: %w", n.ID, err)
	}
	return domain.NotificationID(persisted), nil
}

// timeOrZero returns the underlying time or its zero value when nil.
// nullableTime then handles the zero-to-nil coercion for SQL.
func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ListNotificationsForUser returns notifications ordered by created_at DESC.
func (r *NotificationRepo) CountUnreadForUser(ctx context.Context, userID domain.UserID) (int, error) {
	db := dbtx(ctx, r.pool)
	var n int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM notification_events WHERE user_id = $1 AND NOT read`, userID.UUID(),
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return n, nil
}

func (r *NotificationRepo) ListNotificationsForUser(
	ctx context.Context,
	userID domain.UserID,
	unreadOnly bool,
	limit, offset int,
) ([]domain.Notification, error) {
	if limit <= 0 {
		limit = 50
	}
	db := dbtx(ctx, r.pool)

	q := `SELECT ` + notificationColumns + `
	        FROM notification_events
	       WHERE user_id = $1`
	args := []any{userID.UUID()}
	if unreadOnly {
		q += ` AND read = false`
	}
	q += ` ORDER BY created_at DESC LIMIT $2 OFFSET $3`
	args = append(args, limit, offset)

	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list notifications for %s: %w", userID, err)
	}
	return pgCollectRows(rows, scanNotificationRow, "scan notification row", "iterate notifications")
}

// MarkRead sets read = true and read_at = now() for IDs owned by userID.
// The user_id predicate stops a malicious caller from flipping someone
// else's notifications.
func (r *NotificationRepo) MarkRead(
	ctx context.Context,
	userID domain.UserID,
	ids []domain.NotificationID,
) error {
	if len(ids) == 0 {
		return nil
	}
	db := dbtx(ctx, r.pool)

	uuids := make([]uuid.UUID, len(ids))
	for i, id := range ids {
		uuids[i] = id.UUID()
	}

	_, err := db.Exec(ctx,
		`UPDATE notification_events
		    SET read = true, read_at = now()
		  WHERE user_id = $1 AND id = ANY($2) AND read = false`,
		userID.UUID(), uuids,
	)
	if err != nil {
		return fmt.Errorf("mark notifications read: %w", err)
	}
	return nil
}

// MarkAllReadForUser flips read = true for every unread row of the user.
// Returns the count of rows actually mutated (rows that were already
// read don't contribute).
func (r *NotificationRepo) MarkAllReadForUser(
	ctx context.Context,
	userID domain.UserID,
) (int, error) {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE notification_events
		    SET read = true, read_at = now()
		  WHERE user_id = $1 AND read = false`,
		userID.UUID(),
	)
	if err != nil {
		return 0, fmt.Errorf("mark all read: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// GetByID returns one notification scoped to its owning user. Cross-user
// reads surface as ErrNotFound (privacy: never confirm "exists but
// not yours" vs. "doesn't exist").
func (r *NotificationRepo) GetByID(
	ctx context.Context,
	userID domain.UserID,
	id domain.NotificationID,
) (domain.Notification, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT `+notificationColumns+`
		   FROM notification_events
		  WHERE id = $1 AND user_id = $2`,
		id.UUID(), userID.UUID(),
	)
	n, err := scanNotificationRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Notification{}, fmt.Errorf("notification %s: %w", id, domain.ErrNotFound)
		}
		return domain.Notification{}, fmt.Errorf("get notification: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Row scanner
// ---------------------------------------------------------------------------

// notificationSaveSQL coalesces or inserts one event. The CTE structure:
//
//	existing  - finds the most-recent matching event in [now-24h, now]
//	bumped    - UPDATEs coalesce_count + i18n_params on hit
//	INSERT    - fires only if no row was bumped
//
// The empty-dedup_key fast path skips coalescing — no rows ever match
// because the existing-index excludes empty dedup_key anyway, so
// the existing CTE returns empty and the INSERT always proceeds.
const notificationSaveSQL = `
		WITH existing AS (
			SELECT id, i18n_params
			  FROM notification_events
			 WHERE user_id = $2
			   AND type = $3
			   AND dedup_key = $12
			   AND dedup_key != ''
			   AND created_at > now() - INTERVAL '24 hours'
			 ORDER BY created_at DESC
			 LIMIT 1
		),
		bumped AS (
			UPDATE notification_events ne
			   SET coalesce_count = ne.coalesce_count + 1,
			       i18n_params    = ne.i18n_params || $7::jsonb,
			       updated_at     = COALESCE($17, now())
			  FROM existing e
			 WHERE ne.id = e.id
		 RETURNING ne.id
		),
		inserted AS (
			INSERT INTO notification_events (
				id, user_id, type, severity,
				title_i18n_key, body_i18n_key, i18n_params,
				activity_id, segment_id, external_account_id, worker_name,
				dedup_key, coalesce_count,
				read, read_at,
				created_at, updated_at
			)
			SELECT
				$1, $2, $3, $4,
				$5, $6, $7::jsonb,
				$8, $9, $10, $11,
				$12, $13,
				$14, $15,
				COALESCE($16, now()), COALESCE($17, now())
			WHERE NOT EXISTS (SELECT 1 FROM bumped)
			RETURNING id
		)
		SELECT id FROM bumped
		UNION ALL
		SELECT id FROM inserted
		`

func scanNotificationRow(row rowScanner) (domain.Notification, error) {
	var (
		n                                domain.Notification
		id, userID                       uuid.UUID
		notifType                        int
		severity                         string
		i18nParams                       []byte
		activityID, segmentID, extAcctID *uuid.UUID
		workerName                       *string
	)
	if err := row.Scan(
		&id, &userID, &notifType, &severity,
		&n.TitleI18nKey, &n.BodyI18nKey, &i18nParams,
		&activityID, &segmentID, &extAcctID, &workerName,
		&n.DedupKey, &n.CoalesceCount,
		&n.Read, &n.ReadAt,
		&n.CreatedAt, &n.UpdatedAt,
	); err != nil {
		return domain.Notification{}, err
	}

	n.ID = domain.NotificationID(id)
	n.UserID = domain.UserID(userID)
	n.Type = domain.NotificationType(notifType)
	n.Severity = domain.NotificationSeverity(severity)
	if len(i18nParams) > 0 {
		var parsed map[string]string
		if err := json.Unmarshal(i18nParams, &parsed); err == nil {
			n.I18nParams = parsed
		}
	}
	if activityID != nil {
		a := domain.ActivityID(*activityID)
		n.ActivityID = &a
	}
	if segmentID != nil {
		s := domain.SegmentID(*segmentID)
		n.SegmentID = &s
	}
	if extAcctID != nil {
		ea := domain.ExternalAccountID(*extAcctID)
		n.ExternalAccountID = &ea
	}
	if workerName != nil {
		n.WorkerName = *workerName
	}
	return n, nil
}
