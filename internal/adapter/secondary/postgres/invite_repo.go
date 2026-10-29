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
)

// InviteRepo implements port.InviteRepo on the invites table (migration 3).
type InviteRepo struct {
	pool *pgxpool.Pool
}

func NewInviteRepo(pool *pgxpool.Pool) *InviteRepo {
	return &InviteRepo{pool: pool}
}

const inviteCols = `id, code_prefix, email, assigned_role, created_by_user_id,
	created_at, expires_at, used_at, used_by_user_id, revoked_at`

func (r *InviteRepo) Create(ctx context.Context, inv domain.Invite, codeHash []byte) (domain.InviteID, error) {
	var createdBy *uuid.UUID
	if inv.CreatedByUser != nil {
		u := inv.CreatedByUser.UUID()
		createdBy = &u
	}
	var email *string
	if inv.Email != "" {
		email = &inv.Email
	}
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO invites (code_hash, code_prefix, email, assigned_role, created_by_user_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		codeHash, inv.CodePrefix, email, string(inv.AssignedRole), createdBy, inv.ExpiresAt).Scan(&id)
	if err != nil {
		return domain.InviteID{}, fmt.Errorf("create invite: %w", err)
	}
	return domain.InviteID(id), nil
}

func (r *InviteRepo) ClaimByCodeHash(ctx context.Context, codeHash []byte, now time.Time) (domain.Invite, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE invites
		   SET used_at = $2
		 WHERE code_hash = $1
		   AND used_at IS NULL
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > $2)
		RETURNING `+inviteCols, codeHash, now)
	inv, err := scanInvite(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Invite{}, domain.ErrInviteInvalid
	}
	return inv, err
}

func (r *InviteRepo) SetUsedBy(ctx context.Context, id domain.InviteID, userID domain.UserID) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE invites SET used_by_user_id = $2 WHERE id = $1`, id.UUID(), userID.UUID())
	if err != nil {
		return fmt.Errorf("set invite used_by: %w", err)
	}
	return nil
}

func (r *InviteRepo) Release(ctx context.Context, id domain.InviteID) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE invites SET used_at = NULL, used_by_user_id = NULL WHERE id = $1`, id.UUID())
	if err != nil {
		return fmt.Errorf("release invite: %w", err)
	}
	return nil
}

func (r *InviteRepo) ListForInstance(ctx context.Context) ([]domain.Invite, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+inviteCols+` FROM invites ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()
	var out []domain.Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (r *InviteRepo) Revoke(ctx context.Context, id domain.InviteID, now time.Time) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE invites SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id.UUID(), now)
	if err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	return nil
}

func scanInvite(row pgx.Row) (domain.Invite, error) {
	var (
		inv       domain.Invite
		id        uuid.UUID
		email     *string
		role      string
		createdBy *uuid.UUID
		usedBy    *uuid.UUID
		expiresAt *time.Time
		usedAt    *time.Time
		revokedAt *time.Time
	)
	if err := row.Scan(&id, &inv.CodePrefix, &email, &role, &createdBy,
		&inv.CreatedAt, &expiresAt, &usedAt, &usedBy, &revokedAt); err != nil {
		return domain.Invite{}, err
	}
	inv.ID = domain.InviteID(id)
	if email != nil {
		inv.Email = *email
	}
	inv.AssignedRole = domain.UserRole(role)
	if createdBy != nil {
		uid := domain.UserID(*createdBy)
		inv.CreatedByUser = &uid
	}
	if usedBy != nil {
		uid := domain.UserID(*usedBy)
		inv.UsedByUser = &uid
	}
	inv.ExpiresAt = expiresAt
	inv.UsedAt = usedAt
	inv.RevokedAt = revokedAt
	return inv, nil
}

func (r *InviteRepo) Get(ctx context.Context, id domain.InviteID) (domain.Invite, error) {
	db := dbtx(ctx, r.pool)
	inv, err := scanInvite(db.QueryRow(ctx, `SELECT `+inviteCols+` FROM invites WHERE id = $1`, id.UUID()))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Invite{}, domain.ErrInviteNotFound
	}
	if err != nil {
		return domain.Invite{}, fmt.Errorf("get invite: %w", err)
	}
	return inv, nil
}

func (r *InviteRepo) ResetCode(ctx context.Context, id domain.InviteID, codeHash []byte, prefix string, now time.Time) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`UPDATE invites SET code_hash = $2, code_prefix = $3
		  WHERE id = $1 AND used_at IS NULL AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $4)`,
		id.UUID(), codeHash, prefix, now)
	if err != nil {
		return fmt.Errorf("reset invite code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrInviteInvalid
	}
	return nil
}
