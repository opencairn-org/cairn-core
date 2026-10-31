//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestSecretRotationRepo(t *testing.T) {
	ctx, pool := requirePool(t)
	repo := NewSecretRotationRepo(pool)
	purpose := domain.SigningKeyPurposeNATSAccount // inactive: the one-active index is untouched
	key, err := NewSigningKeyRepo(pool).CreateKey(ctx, domain.SigningKey{Purpose: purpose, PublicKey: "A" + uuid.NewString(), SeedEncrypted: []byte("old"), CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetSigningKeySeed(ctx, key.ID, []byte("new")); err != nil {
		t.Fatal(err)
	}
	seeds, err := repo.SigningKeySeeds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range seeds {
		if s.ID == key.ID {
			found = s.Purpose == purpose && string(s.Seed) == "new"
		}
	}
	if !found {
		t.Fatalf("rewritten seed not listed: %+v", seeds)
	}
	if _, err := repo.OAuthTokens(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ConnectionSecrets(ctx); err != nil {
		t.Fatal(err)
	}
}
