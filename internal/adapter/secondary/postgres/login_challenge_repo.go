package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// LoginChallengeRepo implements port.LoginChallengeRepo over login_challenges.
type LoginChallengeRepo struct {
	pool *pgxpool.Pool
}

var _ port.LoginChallengeRepo = (*LoginChallengeRepo)(nil)

func NewLoginChallengeRepo(pool *pgxpool.Pool) *LoginChallengeRepo {
	return &LoginChallengeRepo{pool: pool}
}

func (r *LoginChallengeRepo) Create(ctx context.Context, c domain.LoginChallenge) (domain.LoginChallengeID, error) {
	db := dbtx(ctx, r.pool)
	var id uuid.UUID
	purpose := c.Purpose
	if purpose == "" {
		purpose = domain.ChallengeSecondFactor
	}
	err := db.QueryRow(ctx,
		`INSERT INTO login_challenges (user_id, token_hash, expires_at, purpose) VALUES ($1, $2, $3, $4) RETURNING id`,
		c.UserID.UUID(), c.TokenHash, c.ExpiresAt, string(purpose)).Scan(&id)
	if err != nil {
		return domain.LoginChallengeID{}, fmt.Errorf("insert login challenge: %w", err)
	}
	return domain.LoginChallengeID(id), nil
}

func (r *LoginChallengeRepo) GetByTokenHash(ctx context.Context, hash []byte) (domain.LoginChallenge, error) {
	db := dbtx(ctx, r.pool)
	var (
		c       domain.LoginChallenge
		id, uid uuid.UUID
	)
	err := db.QueryRow(ctx,
		`SELECT id, user_id, purpose, token_hash, created_at, expires_at, satisfied_at, consumed_at
		   FROM login_challenges WHERE token_hash = $1`, hash,
	).Scan(&id, &uid, &c.Purpose, &c.TokenHash, &c.CreatedAt, &c.ExpiresAt, &c.SatisfiedAt, &c.ConsumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.LoginChallenge{}, fmt.Errorf("login challenge: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.LoginChallenge{}, fmt.Errorf("select login challenge: %w", err)
	}
	c.ID, c.UserID = domain.LoginChallengeID(id), domain.UserID(uid)
	return c, nil
}

func (r *LoginChallengeRepo) MarkSatisfied(ctx context.Context, id domain.LoginChallengeID, now time.Time) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE login_challenges SET satisfied_at = $2 WHERE id = $1 AND satisfied_at IS NULL AND consumed_at IS NULL AND expires_at > $2`,
		id.UUID(), now)
	if err != nil {
		return fmt.Errorf("mark login challenge satisfied: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return port.ErrAuthTokenInvalid
	}
	return nil
}

func (r *LoginChallengeRepo) Consume(ctx context.Context, hash []byte, now time.Time) (domain.UserID, error) {
	db := dbtx(ctx, r.pool)
	var uid uuid.UUID
	err := db.QueryRow(ctx,
		`UPDATE login_challenges SET consumed_at = $2
		  WHERE token_hash = $1 AND satisfied_at IS NOT NULL AND consumed_at IS NULL AND expires_at > $2
		 RETURNING user_id`, hash, now).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.UserID{}, port.ErrAuthTokenInvalid
	}
	if err != nil {
		return domain.UserID{}, fmt.Errorf("consume login challenge: %w", err)
	}
	return domain.UserID(uid), nil
}
