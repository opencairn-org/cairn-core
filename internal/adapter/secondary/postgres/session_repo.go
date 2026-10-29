package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// SessionRepo implements port.SessionRepo against migration 00002's
// sessions table.
type SessionRepo struct {
	pool *pgxpool.Pool
}

func NewSessionRepo(pool *pgxpool.Pool) *SessionRepo {
	return &SessionRepo{pool: pool}
}

func (r *SessionRepo) Create(ctx context.Context, s domain.Session) (domain.SessionID, error) {
	db := dbtx(ctx, r.pool)

	if !s.AuthMethod.Valid() {
		return domain.SessionID{}, fmt.Errorf("invalid auth_method %q", s.AuthMethod)
	}
	if len(s.TokenHash) != 32 {
		return domain.SessionID{}, fmt.Errorf("token_hash must be 32 bytes, got %d", len(s.TokenHash))
	}

	var ip any
	if s.IPAddress.IsValid() {
		ip = s.IPAddress.String()
	}

	var id uuid.UUID
	err := db.QueryRow(ctx,
		`INSERT INTO sessions
		    (user_id, token_hash, auth_method,
		     user_agent_summary, ip_address, ip_geo_summary,
		     expires_at)
		 VALUES ($1, $2, $3, $4, $5::inet, $6, $7)
		 RETURNING id`,
		uuid.UUID(s.UserID),
		s.TokenHash,
		string(s.AuthMethod),
		nullString(s.UserAgentSummary),
		ip,
		nullString(s.IPGeoSummary),
		s.ExpiresAt,
	).Scan(&id)
	if err != nil {
		return domain.SessionID{}, fmt.Errorf("insert session: %w", err)
	}
	return domain.SessionID(id), nil
}

const sessionColumns = `id, user_id, token_hash, auth_method,
		        user_agent_summary, host(ip_address)::text, ip_geo_summary,
		        created_at, last_seen_at, expires_at, revoked_at`

func (r *SessionRepo) GetByTokenHash(ctx context.Context, hash []byte) (domain.Session, error) {
	db := dbtx(ctx, r.pool)
	out, err := scanSession(db.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE token_hash = $1`, hash))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Session{}, fmt.Errorf("session: %w", domain.ErrNotFound)
		}
		return domain.Session{}, fmt.Errorf("select session: %w", err)
	}
	return out, nil
}

func (r *SessionRepo) ListForUser(ctx context.Context, userID domain.UserID) ([]domain.Session, error) {
	db := dbtx(ctx, r.pool)
	rows, err := db.Query(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		  WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
		  ORDER BY last_seen_at DESC`, uuid.UUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	out := []domain.Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sess.TokenHash = nil // never hand hashes to callers listing sessions
		out = append(out, sess)
	}
	return out, rows.Err()
}

// scanSession reads one sessionColumns row.
func scanSession(row pgx.Row) (domain.Session, error) {
	var (
		id, userID uuid.UUID
		tokenHash  []byte
		authMethod string
		uaSummary  *string
		ipString   *string
		geoSummary *string
		createdAt  time.Time
		lastSeenAt time.Time
		expiresAt  time.Time
		revokedAt  *time.Time
	)
	if err := row.Scan(&id, &userID, &tokenHash, &authMethod,
		&uaSummary, &ipString, &geoSummary,
		&createdAt, &lastSeenAt, &expiresAt, &revokedAt); err != nil {
		return domain.Session{}, err
	}
	out := domain.Session{
		ID:               domain.SessionID(id),
		UserID:           domain.UserID(userID),
		TokenHash:        tokenHash,
		AuthMethod:       domain.SessionAuthMethod(authMethod),
		UserAgentSummary: deref(uaSummary),
		IPGeoSummary:     deref(geoSummary),
		CreatedAt:        createdAt,
		LastSeenAt:       lastSeenAt,
		ExpiresAt:        expiresAt,
		RevokedAt:        revokedAt,
	}
	if ipString != nil {
		if addr, err := netip.ParseAddr(*ipString); err == nil {
			out.IPAddress = addr
		}
	}
	return out, nil
}

func (r *SessionRepo) TouchLastSeen(ctx context.Context, id domain.SessionID) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE sessions SET last_seen_at = now() WHERE id = $1 AND revoked_at IS NULL`,
		uuid.UUID(id),
	)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

func (r *SessionRepo) Revoke(ctx context.Context, id domain.SessionID) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`,
		uuid.UUID(id),
	)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

func (r *SessionRepo) RevokeAllForUser(ctx context.Context, userID domain.UserID) error {
	db := dbtx(ctx, r.pool)
	_, err := db.Exec(ctx,
		`UPDATE sessions SET revoked_at = now()
		  WHERE user_id = $1 AND revoked_at IS NULL`,
		uuid.UUID(userID),
	)
	if err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

// nullString returns nil for an empty string so the DB stores NULL
// rather than the empty string. Pure ergonomics — the columns are
// nullable anyway.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// nullIfEmpty stores "" as SQL NULL for nullable text columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
