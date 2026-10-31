package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// OAuthTokenCiphertexts is one external account's encrypted token pair.
type OAuthTokenCiphertexts struct {
	ID              domain.ExternalAccountID
	Access, Refresh []byte
}

// ConnectionSecretCiphertext is one connection's encrypted client secret.
type ConnectionSecretCiphertext struct {
	ID     domain.ConnectionID
	Secret []byte
}

// SigningKeySeedCiphertext is one signing key's encrypted seed.
type SigningKeySeedCiphertext struct {
	ID      domain.SigningKeyID
	Purpose string
	Seed    []byte
}

// SecretRotationRepo reads and rewrites every secret-at-rest for a master-key rotation.
type SecretRotationRepo interface {
	OAuthTokens(ctx context.Context) ([]OAuthTokenCiphertexts, error)
	SetOAuthTokens(ctx context.Context, id domain.ExternalAccountID, access, refresh []byte) error
	ConnectionSecrets(ctx context.Context) ([]ConnectionSecretCiphertext, error)
	SetConnectionSecret(ctx context.Context, id domain.ConnectionID, secret []byte) error
	SigningKeySeeds(ctx context.Context) ([]SigningKeySeedCiphertext, error)
	SetSigningKeySeed(ctx context.Context, id domain.SigningKeyID, seed []byte) error
}
