//go:build integration

package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestCustomBestEffortRepo(t *testing.T) {
	ctx, pool := requirePool(t)
	users := NewUserRepo(pool)
	uid := domain.UserID(uuid.New())
	suffix := uid.String()[:8]
	if err := users.CreateUser(ctx, domain.User{
		ID: uid, Username: "cbe-" + suffix, Email: "cbe-" + suffix + "@example.com",
		DisplayName: "CBE", Role: domain.UserRoleUser, Status: domain.UserStatusActive, Units: domain.UserUnitsMetric,
	}, domain.Credentials{UserID: uid}); err != nil {
		t.Fatal(err)
	}
	repo := NewCustomBestEffortRepo(pool)
	in := domain.CustomBestEffort{UserID: uid, ActivityType: domain.ActivityTypeRun, Metric: domain.BestEffortMetricPace, WindowKind: domain.BestEffortWindowDistance, WindowValue: 7500, Label: "loop"}
	id, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, in); !errors.Is(err, domain.ErrCustomEffortDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	list, err := repo.ListForUser(ctx, uid, domain.ActivityTypeRun)
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Label != "loop" {
		t.Fatalf("list: %v %+v", err, list)
	}
	if list, _ := repo.ListForUser(ctx, uid, domain.ActivityTypeRide); len(list) != 0 {
		t.Fatalf("type filter: %+v", list)
	}
	if err := repo.Delete(ctx, domain.UserID(uuid.New()), id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign delete: %v", err)
	}
	if err := repo.Delete(ctx, uid, id); err != nil {
		t.Fatal(err)
	}
	if err := NewBestEffortRepo(pool).DeleteForUserWindow(ctx, uid, domain.ActivityTypeRun, domain.BestEffortMetricPace, domain.BestEffortWindowDistance, 7500); err != nil {
		t.Fatal(err)
	}
}
