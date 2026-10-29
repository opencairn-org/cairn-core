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

// DeadLetterRepo persists rows from the cairn_dead_lettered_jobs
// advisory subscriber. The port (in internal/port/dead_letter.go would
// add coupling for a single caller) lives inline here; admin endpoints
// use this struct directly.
type DeadLetterRepo struct {
	pool *pgxpool.Pool
}

func NewDeadLetterRepo(pool *pgxpool.Pool) *DeadLetterRepo {
	return &DeadLetterRepo{pool: pool}
}

// Capture upserts a DLQ row keyed on (stream, subject, msg_id). On
// duplicate, bumps last_seen_at and delivered_count — useful when a
// flapping job hits MaxDeliver repeatedly across retries.
func (r *DeadLetterRepo) Capture(ctx context.Context, j domain.DeadLetteredJob) error {
	db := dbtx(ctx, r.pool)

	headersJSON, err := json.Marshal(j.Headers)
	if err != nil {
		return fmt.Errorf("marshal headers: %w", err)
	}
	id := j.ID.UUID()
	if id == uuid.Nil {
		if newID, err := uuid.NewV7(); err == nil {
			id = newID
		} else {
			id = uuid.New()
		}
	}
	now := time.Now().UTC()
	if !j.FirstSeenAt.IsZero() {
		now = j.FirstSeenAt
	}

	_, err = db.Exec(ctx,
		`INSERT INTO dead_lettered_jobs (
		    id, stream, subject, consumer, msg_id,
		    payload, headers, delivered_count, last_error,
		    first_seen_at, last_seen_at
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $10)
		 ON CONFLICT (stream, subject, msg_id) DO UPDATE
		    SET delivered_count = EXCLUDED.delivered_count,
		        last_error      = EXCLUDED.last_error,
		        last_seen_at    = EXCLUDED.last_seen_at,
		        payload         = COALESCE(EXCLUDED.payload, dead_lettered_jobs.payload)`,
		id, j.Stream, j.Subject, j.Consumer, nullableString(j.MsgID),
		nullableBytes(j.Payload), string(headersJSON), j.DeliveredCount, j.LastError,
		now,
	)
	if err != nil {
		return fmt.Errorf("capture dead-letter: %w", err)
	}
	return nil
}

// DLQListInput parameterises the admin /admin/dlq listing.
// DLQListInput is the filter of List (port.DeadLetterFilter).
type DLQListInput = port.DeadLetterFilter

func (r *DeadLetterRepo) List(ctx context.Context, in DLQListInput) ([]domain.DeadLetteredJob, error) {
	db := dbtx(ctx, r.pool)
	if in.Limit <= 0 || in.Limit > 200 {
		in.Limit = 50
	}

	q := `SELECT id, stream, subject, consumer, msg_id,
	             payload, headers, delivered_count, last_error,
	             first_seen_at, last_seen_at,
	             replayed_at, replayed_by_user_id, replay_count
	        FROM dead_lettered_jobs
	       WHERE ($1::text IS NULL OR stream = $1)
	         AND ($2::text IS NULL OR subject = $2)
	         AND ($3::bool OR replayed_at IS NULL)
	         AND ($4::timestamptz IS NULL OR first_seen_at < $4)
	    ORDER BY first_seen_at DESC
	       LIMIT $5`

	rows, err := db.Query(ctx, q,
		nullableString(in.Stream),
		nullableString(in.Subject),
		in.IncludeReplayed,
		nullableTime(in.BeforeFirstSeenAt),
		in.Limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list dead-lettered: %w", err)
	}
	defer rows.Close()

	var out []domain.DeadLetteredJob
	for rows.Next() {
		j, err := scanDLQRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Get loads a single row.
func (r *DeadLetterRepo) Get(ctx context.Context, id domain.DeadLetteredJobID) (domain.DeadLetteredJob, error) {
	db := dbtx(ctx, r.pool)
	row := db.QueryRow(ctx,
		`SELECT id, stream, subject, consumer, msg_id,
		        payload, headers, delivered_count, last_error,
		        first_seen_at, last_seen_at,
		        replayed_at, replayed_by_user_id, replay_count
		   FROM dead_lettered_jobs
		  WHERE id = $1`,
		id.UUID(),
	)
	j, err := scanDLQRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DeadLetteredJob{}, domain.ErrNotFound
	}
	return j, err
}

// MarkReplayed stamps the row and increments replay_count. Caller has
// already republished the payload to NATS; this is just the audit
// breadcrumb.
func (r *DeadLetterRepo) MarkReplayed(
	ctx context.Context,
	id domain.DeadLetteredJobID,
	by *domain.UserID,
	at time.Time,
) error {
	db := dbtx(ctx, r.pool)
	var actorID any
	if by != nil {
		actorID = by.UUID()
	}
	_, err := db.Exec(ctx,
		`UPDATE dead_lettered_jobs
		    SET replayed_at         = $2,
		        replayed_by_user_id = $3,
		        replay_count        = replay_count + 1
		  WHERE id = $1`,
		id.UUID(), at, actorID,
	)
	if err != nil {
		return fmt.Errorf("mark replayed %s: %w", id, err)
	}
	return nil
}

func scanDLQRow(row rowScanner) (domain.DeadLetteredJob, error) {
	var (
		id              uuid.UUID
		stream, subject string
		consumer        string
		msgID           *string
		payload         []byte
		headersRaw      []byte
		deliveredCount  int
		lastError       string
		firstSeenAt     time.Time
		lastSeenAt      time.Time
		replayedAt      *time.Time
		replayedByRaw   uuid.NullUUID
		replayCount     int
	)
	if err := row.Scan(
		&id, &stream, &subject, &consumer, &msgID,
		&payload, &headersRaw, &deliveredCount, &lastError,
		&firstSeenAt, &lastSeenAt,
		&replayedAt, &replayedByRaw, &replayCount,
	); err != nil {
		return domain.DeadLetteredJob{}, err
	}
	headers := map[string]string{}
	if len(headersRaw) > 0 {
		_ = json.Unmarshal(headersRaw, &headers)
	}
	j := domain.DeadLetteredJob{
		ID:             domain.DeadLetteredJobID(id),
		Stream:         stream,
		Subject:        subject,
		Consumer:       consumer,
		Payload:        payload,
		Headers:        headers,
		DeliveredCount: deliveredCount,
		LastError:      lastError,
		FirstSeenAt:    firstSeenAt,
		LastSeenAt:     lastSeenAt,
		ReplayedAt:     replayedAt,
		ReplayCount:    replayCount,
	}
	if msgID != nil {
		j.MsgID = *msgID
	}
	if replayedByRaw.Valid {
		u := domain.UserID(replayedByRaw.UUID)
		j.ReplayedByUserID = &u
	}
	return j, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (r *DeadLetterRepo) CountOpen(ctx context.Context) (int, error) {
	db := dbtx(ctx, r.pool)
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM dead_lettered_jobs WHERE replayed_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count dead-lettered: %w", err)
	}
	return n, nil
}

var _ port.DeadLetterRepo = (*DeadLetterRepo)(nil)
