package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// SecretRotationRepo implements port.SecretRotationRepo.
type SecretRotationRepo struct{ pool *pgxpool.Pool }

func NewSecretRotationRepo(pool *pgxpool.Pool) *SecretRotationRepo {
	return &SecretRotationRepo{pool: pool}
}

func (r *SecretRotationRepo) OAuthTokens(ctx context.Context) ([]port.OAuthTokenCiphertexts, error) {
	rows, err := dbtx(ctx, r.pool).Query(ctx, `SELECT id, access_token_encrypted, refresh_token_encrypted FROM external_accounts`)
	if err != nil {
		return nil, err
	}
	return pgCollectRows(rows, func(row rowScanner) (port.OAuthTokenCiphertexts, error) {
		var (
			id  uuid.UUID
			out port.OAuthTokenCiphertexts
		)
		err := row.Scan(&id, &out.Access, &out.Refresh)
		out.ID = domain.ExternalAccountID(id)
		return out, err
	}, "scan oauth tokens", "iterate oauth tokens")
}

func (r *SecretRotationRepo) SetOAuthTokens(ctx context.Context, id domain.ExternalAccountID, access, refresh []byte) error {
	_, err := dbtx(ctx, r.pool).Exec(ctx,
		`UPDATE external_accounts SET access_token_encrypted=$1, refresh_token_encrypted=$2 WHERE id=$3`, access, refresh, id.UUID())
	return err
}

func (r *SecretRotationRepo) ConnectionSecrets(ctx context.Context) ([]port.ConnectionSecretCiphertext, error) {
	rows, err := dbtx(ctx, r.pool).Query(ctx, `SELECT id, client_secret_encrypted FROM user_provider_configs`)
	if err != nil {
		return nil, err
	}
	return pgCollectRows(rows, func(row rowScanner) (port.ConnectionSecretCiphertext, error) {
		var (
			id  uuid.UUID
			out port.ConnectionSecretCiphertext
		)
		err := row.Scan(&id, &out.Secret)
		out.ID = domain.ConnectionID(id)
		return out, err
	}, "scan connection secrets", "iterate connection secrets")
}

func (r *SecretRotationRepo) SetConnectionSecret(ctx context.Context, id domain.ConnectionID, secret []byte) error {
	_, err := dbtx(ctx, r.pool).Exec(ctx, `UPDATE user_provider_configs SET client_secret_encrypted=$1 WHERE id=$2`, secret, id.UUID())
	return err
}

func (r *SecretRotationRepo) SigningKeySeeds(ctx context.Context) ([]port.SigningKeySeedCiphertext, error) {
	rows, err := dbtx(ctx, r.pool).Query(ctx, `SELECT id, purpose, seed_encrypted FROM instance_signing_keys`)
	if err != nil {
		return nil, err
	}
	return pgCollectRows(rows, func(row rowScanner) (port.SigningKeySeedCiphertext, error) {
		var (
			id  uuid.UUID
			out port.SigningKeySeedCiphertext
		)
		err := row.Scan(&id, &out.Purpose, &out.Seed)
		out.ID = domain.SigningKeyID(id)
		return out, err
	}, "scan signing key seeds", "iterate signing key seeds")
}

func (r *SecretRotationRepo) SetSigningKeySeed(ctx context.Context, id domain.SigningKeyID, seed []byte) error {
	_, err := dbtx(ctx, r.pool).Exec(ctx, `UPDATE instance_signing_keys SET seed_encrypted=$1 WHERE id=$2`, seed, id.UUID())
	return err
}
