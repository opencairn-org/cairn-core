package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ---- device authorization (RFC 8628, migration 00072) ----

const deviceCodeColumns = `client_id, user_code, scope, status, user_id, granted_scope, expires_at,
	decided_at, consumed_at, last_polled_at, created_at`

func (r *OAuthServerRepo) CreateDeviceCode(ctx context.Context, d domain.OAuthDeviceCode, deviceCodeHash []byte) error {
	// Expired rows only block user-code reuse; drop them before inserting.
	if _, err := r.pool.Exec(ctx, `DELETE FROM oauth_device_codes WHERE expires_at < $1`, time.Now().UTC()); err != nil {
		return fmt.Errorf("purge device codes: %w", err)
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO oauth_device_codes (device_code_hash, user_code, client_id, scope, expires_at)
		VALUES ($1,$2,$3,$4,$5)`,
		deviceCodeHash, d.UserCode, d.ClientID, d.Scope, d.ExpiresAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("device code: %w", domain.ErrUnique)
	}
	if err != nil {
		return fmt.Errorf("create device code: %w", err)
	}
	return nil
}

func (r *OAuthServerRepo) FindDeviceCodeByUserCode(ctx context.Context, userCode string, now time.Time) (domain.OAuthDeviceCode, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+deviceCodeColumns+`
		FROM oauth_device_codes WHERE user_code = $1 AND consumed_at IS NULL AND expires_at > $2`, userCode, now)
	return scanDeviceCode(row)
}

func (r *OAuthServerRepo) DecideDeviceCode(ctx context.Context, userCode string, userID domain.UserID, approve bool, grantedScope string, now time.Time) error {
	status := domain.DeviceCodeDenied
	if approve {
		status = domain.DeviceCodeApproved
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE oauth_device_codes
		   SET status = $3, user_id = $4, granted_scope = $5, decided_at = $2
		 WHERE user_code = $1 AND status = 'pending' AND consumed_at IS NULL AND expires_at > $2`,
		userCode, now, string(status), userID.UUID(), grantedScope)
	if err != nil {
		return fmt.Errorf("decide device code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("device code: %w", domain.ErrNotFound)
	}
	return nil
}

func (r *OAuthServerRepo) PollDeviceCode(ctx context.Context, deviceCodeHash []byte, now time.Time) (domain.OAuthDeviceCode, error) {
	// prev carries the poll time before this one so the caller can apply slow_down.
	row := r.pool.QueryRow(ctx, `
		WITH prev AS (
			SELECT device_code_hash, last_polled_at FROM oauth_device_codes
			 WHERE device_code_hash = $1 AND consumed_at IS NULL
		)
		UPDATE oauth_device_codes d SET last_polled_at = $2
		  FROM prev WHERE d.device_code_hash = prev.device_code_hash
		RETURNING d.client_id, d.user_code, d.scope, d.status, d.user_id, d.granted_scope, d.expires_at,
		          d.decided_at, d.consumed_at, prev.last_polled_at, d.created_at`, deviceCodeHash, now)
	return scanDeviceCode(row)
}

func (r *OAuthServerRepo) ConsumeDeviceCode(ctx context.Context, deviceCodeHash []byte, now time.Time) (domain.OAuthDeviceCode, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE oauth_device_codes SET consumed_at = $2
		 WHERE device_code_hash = $1 AND status = 'approved' AND consumed_at IS NULL AND expires_at > $2
		RETURNING `+deviceCodeColumns, deviceCodeHash, now)
	return scanDeviceCode(row)
}

func scanDeviceCode(row pgx.Row) (domain.OAuthDeviceCode, error) {
	var (
		d      domain.OAuthDeviceCode
		status string
		uid    *uuid.UUID
	)
	err := row.Scan(&d.ClientID, &d.UserCode, &d.Scope, &status, &uid, &d.GrantedScope, &d.ExpiresAt,
		&d.DecidedAt, &d.ConsumedAt, &d.LastPolledAt, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.OAuthDeviceCode{}, fmt.Errorf("device code: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.OAuthDeviceCode{}, err
	}
	d.Status = domain.DeviceCodeStatus(status)
	if uid != nil {
		id := domain.UserID(*uid)
		d.UserID = &id
	}
	return d, nil
}
