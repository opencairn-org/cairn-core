//go:build integration

package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestMaintenanceJobRepo(t *testing.T) {
	ctx, pool := requirePool(t)
	admin := seedUser(t)
	repo := NewMaintenanceJobRepo(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	older := domain.MaintenanceJob{ID: uuid.NewString(), Operation: domain.MaintenancePurgeSoftDeleted, Status: domain.MaintenanceSucceeded,
		Estimated: 3, Processed: 3, QueuedAt: now.Add(-time.Hour), StartedAt: &now, FinishedAt: &now}
	queued := domain.MaintenanceJob{ID: uuid.NewString(), Operation: domain.MaintenanceRecomputeTrainingLoad, UserIDs: []domain.UserID{admin},
		Status: domain.MaintenanceQueued, Estimated: 1, TriggeredBy: &admin, QueuedAt: now}
	for _, job := range []domain.MaintenanceJob{older, queued} {
		if err := repo.SaveJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.GetJob(ctx, queued.ID)
	if err != nil || got.Status != domain.MaintenanceQueued || len(got.UserIDs) != 1 || got.UserIDs[0] != admin || got.TriggeredBy == nil || *got.TriggeredBy != admin || !got.QueuedAt.Equal(now) {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := repo.GetJob(ctx, uuid.NewString()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := repo.GetJob(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("malformed id: %v", err)
	}
	open, err := repo.ListOpenJobs(ctx)
	if err != nil || len(open) < 1 || open[len(open)-1].ID != queued.ID {
		t.Fatalf("open: %v %+v", err, open)
	}
	queued.Status, queued.Processed, queued.Error, queued.FinishedAt = domain.MaintenanceFailed, 1, "boom", &now
	if err := repo.SaveJob(ctx, queued); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetJob(ctx, queued.ID); got.Status != domain.MaintenanceFailed || got.Error != "boom" || got.Processed != 1 || got.FinishedAt == nil {
		t.Fatalf("update: %+v", got)
	}
	all, err := repo.ListJobs(ctx, 50)
	if err != nil || len(all) < 2 {
		t.Fatalf("list: %v %d", err, len(all))
	}
	seen := map[string]int{}
	for i, job := range all {
		seen[job.ID] = i
	}
	if _, ok := seen[queued.ID]; !ok || seen[queued.ID] > seen[older.ID] {
		t.Fatalf("newest first: %v", seen)
	}
}
