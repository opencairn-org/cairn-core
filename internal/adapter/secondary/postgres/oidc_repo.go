package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// OIDC providers are env-configured + held in memory (no DB rows). The only
// persisted OIDC state is the user↔provider link below, keyed on the provider
// id string (linked_oidc_identities.provider).

// ---------------------------------------------------------------------------
// LinkedIdentityRepo
// ---------------------------------------------------------------------------

type LinkedIdentityRepo struct {
	pool *pgxpool.Pool
}

func NewLinkedIdentityRepo(pool *pgxpool.Pool) *LinkedIdentityRepo {
	return &LinkedIdentityRepo{pool: pool}
}

func (r *LinkedIdentityRepo) FindBySubject(
	ctx context.Context,
	provider, subject string,
) (domain.LinkedIdentity, error) {
	db := dbtx(ctx, r.pool)

	var (
		id, userID    uuid.UUID
		prov, sub     string
		email         string
		lastClaimsRaw []byte
		linkedAt      time.Time
		lastUsedAt    *time.Time
	)
	err := db.QueryRow(ctx,
		`SELECT id, user_id, provider, subject, COALESCE(email::text, ''),
		        COALESCE(last_claims::text, '{}')::bytea,
		        linked_at, last_used_at
		   FROM linked_oidc_identities
		  WHERE provider = $1 AND subject = $2`,
		provider, subject,
	).Scan(&id, &userID, &prov, &sub, &email, &lastClaimsRaw, &linkedAt, &lastUsedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.LinkedIdentity{},
				fmt.Errorf("linked identity (%s, %s): %w", provider, subject, domain.ErrNotFound)
		}
		return domain.LinkedIdentity{}, fmt.Errorf("select linked identity: %w", err)
	}

	out := domain.LinkedIdentity{
		ID:         domain.LinkedIdentityID(id),
		UserID:     domain.UserID(userID),
		Provider:   prov,
		Subject:    sub,
		Email:      email,
		LinkedAt:   linkedAt,
		LastUsedAt: lastUsedAt,
	}
	if len(lastClaimsRaw) > 0 {
		_ = json.Unmarshal(lastClaimsRaw, &out.LastClaims)
	}
	return out, nil
}

func (r *LinkedIdentityRepo) Create(ctx context.Context, li domain.LinkedIdentity) (domain.LinkedIdentityID, error) {
	db := dbtx(ctx, r.pool)
	claims := []byte("{}")
	if len(li.LastClaims) > 0 {
		b, err := json.Marshal(li.LastClaims)
		if err != nil {
			return domain.LinkedIdentityID{}, fmt.Errorf("marshal claims: %w", err)
		}
		claims = b
	}

	var id uuid.UUID
	err := db.QueryRow(ctx,
		`INSERT INTO linked_oidc_identities
		    (user_id, provider, subject, email, last_claims)
		 VALUES ($1, $2, $3, NULLIF($4, '')::citext, $5)
		 RETURNING id`,
		uuid.UUID(li.UserID), li.Provider, li.Subject, li.Email, claims,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.LinkedIdentityID{}, fmt.Errorf("linked identity: %w", domain.ErrUnique)
		}
		return domain.LinkedIdentityID{}, fmt.Errorf("insert linked identity: %w", err)
	}
	return domain.LinkedIdentityID(id), nil
}

func (r *LinkedIdentityRepo) TouchLastUsed(
	ctx context.Context,
	id domain.LinkedIdentityID,
	claims map[string]any,
) error {
	db := dbtx(ctx, r.pool)
	body := []byte("{}")
	if len(claims) > 0 {
		if b, err := json.Marshal(claims); err == nil {
			body = b
		}
	}
	_, err := db.Exec(ctx,
		`UPDATE linked_oidc_identities SET last_used_at = now(), last_claims = $2 WHERE id = $1`,
		uuid.UUID(id), body,
	)
	if err != nil {
		return fmt.Errorf("touch linked identity: %w", err)
	}
	return nil
}

func (r *LinkedIdentityRepo) ListForUser(ctx context.Context, userID domain.UserID) ([]domain.LinkedIdentity, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT id, user_id, provider, subject, COALESCE(email::text, ''), linked_at, last_used_at
		   FROM linked_oidc_identities
		  WHERE user_id = $1
		  ORDER BY linked_at ASC`, uuid.UUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list linked identities: %w", err)
	}
	defer rows.Close()
	out := []domain.LinkedIdentity{}
	for rows.Next() {
		var (
			id, uid  uuid.UUID
			li       domain.LinkedIdentity
			linkedAt time.Time
		)
		if err := rows.Scan(&id, &uid, &li.Provider, &li.Subject, &li.Email, &linkedAt, &li.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan linked identity: %w", err)
		}
		li.ID, li.UserID, li.LinkedAt = domain.LinkedIdentityID(id), domain.UserID(uid), linkedAt
		out = append(out, li)
	}
	return out, rows.Err()
}

func (r *LinkedIdentityRepo) Delete(ctx context.Context, userID domain.UserID, id domain.LinkedIdentityID) error {
	db := dbtx(ctx, r.pool)
	tag, err := db.Exec(ctx,
		`DELETE FROM linked_oidc_identities WHERE id = $1 AND user_id = $2`,
		uuid.UUID(id), uuid.UUID(userID))
	if err != nil {
		return fmt.Errorf("delete linked identity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("linked identity %s: %w", uuid.UUID(id), domain.ErrNotFound)
	}
	return nil
}

// isUniqueViolation reports whether err is a Postgres 23505 unique_violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
