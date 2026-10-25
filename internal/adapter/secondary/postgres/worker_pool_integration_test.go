//go:build integration

package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestWorkerPoolRepo_NameBoundToPackage(t *testing.T) {
	ctx, pool := requirePool(t)
	repo := NewWorkerPoolRepo(pool)
	name := "pool-" + uuid.NewString()[:8]
	if err := repo.Upsert(ctx, domain.WorkerPool{Provider: "strava", Name: name, Package: "pkg-a"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, domain.WorkerPool{Provider: "strava", Name: name, Package: "pkg-a"}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := repo.Upsert(ctx, domain.WorkerPool{Provider: "strava", Name: name, Package: "pkg-b"}); !errors.Is(err, domain.ErrPoolPackageChanged) {
		t.Fatalf("package change: %v", err)
	}
	got, err := repo.Get(ctx, "strava", name)
	if err != nil || got.Package != "pkg-a" {
		t.Fatalf("Get = %+v / %v", got, err)
	}
	list, err := repo.ListForProvider(ctx, "strava")
	if err != nil || len(list) == 0 {
		t.Fatalf("List = %v / %v", list, err)
	}
}
