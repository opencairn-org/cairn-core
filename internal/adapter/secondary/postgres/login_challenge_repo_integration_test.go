//go:build integration

package postgres

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

func TestLoginChallengeRepo(t *testing.T) {
	ctx, pool := requirePool(t)
	users := NewUserRepo(pool)
	uid := domain.UserID(uuid.New())
	suffix := uid.String()[:8]
	if err := users.CreateUser(ctx, domain.User{
		ID: uid, Username: "lc-" + suffix, Email: "lc-" + suffix + "@example.com",
		DisplayName: "LC", Role: domain.UserRoleUser, Status: domain.UserStatusActive, Units: domain.UserUnitsMetric,
	}, domain.Credentials{UserID: uid}); err != nil {
		t.Fatal(err)
	}
	if err := users.SetPasskeySecondFactor(ctx, uid, true); err != nil {
		t.Fatal(err)
	}
	if u, _ := users.GetUser(ctx, uid); !u.PasskeySecondFactor {
		t.Fatal("flag not persisted")
	}
	if _, cred, err := users.GetCredentialsByLogin(ctx, "lc-"+suffix); err != nil || cred.UserID != uid {
		t.Fatalf("credentials scan with new column: %v", err)
	}

	repo := NewLoginChallengeRepo(pool)
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte(uuid.NewString()))
	id, err := repo.Create(ctx, domain.LoginChallenge{UserID: uid, TokenHash: hash[:], ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Consume(ctx, hash[:], now); !errors.Is(err, port.ErrAuthTokenInvalid) {
		t.Fatalf("consume before satisfied: %v", err)
	}
	got, err := repo.GetByTokenHash(ctx, hash[:])
	if err != nil || got.ID != id || got.UserID != uid || got.SatisfiedAt != nil {
		t.Fatalf("get: %v %+v", err, got)
	}
	if err := repo.MarkSatisfied(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSatisfied(ctx, id, now); !errors.Is(err, port.ErrAuthTokenInvalid) {
		t.Fatalf("satisfy twice: %v", err)
	}
	if got, err := repo.Consume(ctx, hash[:], now); err != nil || got != uid {
		t.Fatalf("consume: %v %s", err, got)
	}
	if _, err := repo.Consume(ctx, hash[:], now); !errors.Is(err, port.ErrAuthTokenInvalid) {
		t.Fatalf("replay: %v", err)
	}
	if _, err := repo.GetByTokenHash(ctx, []byte("nope")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}
