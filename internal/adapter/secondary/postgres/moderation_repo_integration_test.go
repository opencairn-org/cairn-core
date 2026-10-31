//go:build integration

package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestModerationRepo_SetActivityHidden_Integration(t *testing.T) {
	ctx, pool := requirePool(t)
	activities := NewActivityRepo(pool)
	repo := NewModerationRepo(pool)
	id := seedGearActivity(t, ctx, activities, seedUser(t), nil, 1000, 600)

	if err := repo.SetActivityHidden(ctx, id, true); err != nil {
		t.Fatalf("hide: %v", err)
	}
	got, err := activities.GetActivity(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HiddenByAdmin {
		t.Fatal("activity not hidden")
	}
	// Re-applying the same state still matches the row.
	if err := repo.SetActivityHidden(ctx, id, true); err != nil {
		t.Fatalf("hide again: %v", err)
	}
	if err := repo.SetActivityHidden(ctx, domain.ActivityID(uuid.New()), true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown id: want ErrNotFound, got %v", err)
	}
}
