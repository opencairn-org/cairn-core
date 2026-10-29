//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func seedMaintenanceActivity(t *testing.T, acts *ActivityRepo, uid domain.UserID, rawBlob string) (domain.ActivityID, domain.SourceID) {
	t.Helper()
	ctx, _ := requirePool(t)
	now := time.Now().UTC()
	actID := domain.ActivityID(uuid.New())
	if err := acts.SaveActivity(ctx, domain.Activity{
		ID: actID, UserID: uid, Type: domain.ActivityTypeRide, Title: "t", StartTime: now, EndTime: now.Add(time.Hour),
		ElapsedDuration: time.Hour, MovingDuration: time.Hour, Timezone: "UTC", MergedAt: now, Privacy: domain.PrivacyPrivate,
	}); err != nil {
		t.Fatalf("save activity: %v", err)
	}
	srcID := domain.SourceID(uuid.New())
	if err := acts.SaveSource(ctx, domain.ActivitySource{
		ID: srcID, ActivityID: actID, UserID: uid, Provider: "upload", ExternalID: "m-" + srcID.String()[:8],
		SourceWorkerName: "w", SourceWorkerVersion: "1.0", SourceWorkerPackage: "p", Status: domain.SourceStatusActive,
		ReimportStatus: domain.ReimportStatusCurrent, ImportedAt: now, UpdatedAt: now,
		Parsed: domain.ActivitySourcePayload{Type: domain.ActivityTypeRide, StartTime: now, EndTime: now.Add(time.Hour), ElapsedDuration: time.Hour, MovingDuration: time.Hour, Timezone: "UTC"},
	}); err != nil {
		t.Fatalf("save source: %v", err)
	}
	if rawBlob != "" {
		if err := acts.SetSourceRawBlob(ctx, srcID, rawBlob, "application/gpx+xml", 10); err != nil {
			t.Fatal(err)
		}
	}
	return actID, srcID
}

func TestMaintenanceRepo(t *testing.T) {
	ctx, pool := requirePool(t)
	uid := seedUser(t)
	acts := NewActivityRepo(pool)
	live, liveSrc := seedMaintenanceActivity(t, acts, uid, "raw/live")
	gone, _ := seedMaintenanceActivity(t, acts, uid, "raw/gone")
	if err := acts.SoftDeleteActivity(ctx, gone, time.Now().Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	repo := NewMaintenanceRepo(pool)
	users := []domain.UserID{uid}

	if n, err := repo.CountActivities(ctx, users); err != nil || n != 1 {
		t.Fatalf("live activities: %d %v", n, err)
	}
	if ids, err := repo.ListActivityIDs(ctx, users, domain.ActivityID{}, 10); err != nil || len(ids) != 1 || ids[0] != live {
		t.Fatalf("activity ids: %v %v", ids, err)
	}
	if n, err := repo.CountSources(ctx, users); err != nil || n != 1 {
		t.Fatalf("live sources: %d %v", n, err)
	}
	if ids, err := repo.ListSourceIDs(ctx, users, domain.SourceID{}, 10); err != nil || len(ids) != 1 || ids[0] != liveSrc {
		t.Fatalf("source ids: %v %v", ids, err)
	}
	if ids, err := repo.ListSourceIDs(ctx, users, liveSrc, 10); err != nil || len(ids) != 0 {
		t.Fatalf("keyset after last: %v %v", ids, err)
	}
	if n, err := repo.CountUsers(ctx, users); err != nil || n != 1 {
		t.Fatalf("users: %d %v", n, err)
	}
	if ids, err := repo.ListUserIDs(ctx, users, domain.UserID{}, 10); err != nil || len(ids) != 1 || ids[0] != uid {
		t.Fatalf("user ids: %v %v", ids, err)
	}
	cut := time.Now().Add(-30 * 24 * time.Hour)
	if n, err := repo.CountSoftDeletedActivities(ctx, users, cut); err != nil || n != 1 {
		t.Fatalf("soft-deleted: %d %v", n, err)
	}
	if ids, err := repo.ListSoftDeletedActivityIDs(ctx, users, cut, domain.ActivityID{}, 10); err != nil || len(ids) != 1 || ids[0] != gone {
		t.Fatalf("soft-deleted ids: %v %v", ids, err)
	}
	refs, err := repo.ReferencedBlobKeys(ctx, []string{"raw/live", "raw/gone", "raw/none"})
	if err != nil || !refs["raw/live"] || !refs["raw/gone"] || refs["raw/none"] {
		t.Fatalf("references: %v %v", err, refs)
	}
	keys, err := repo.HardDeleteActivity(ctx, gone)
	if err != nil || len(keys) != 1 || keys[0] != "raw/gone" {
		t.Fatalf("hard delete: %v %v", err, keys)
	}
	if n, _ := repo.CountSoftDeletedActivities(ctx, users, cut); n != 0 {
		t.Fatalf("still soft-deleted: %d", n)
	}
	// A live activity is never hard-deleted by this path.
	if keys, err := repo.HardDeleteActivity(ctx, live); err != nil || len(keys) != 0 {
		t.Fatalf("live hard delete: %v %v", keys, err)
	}
	if n, _ := repo.CountActivities(ctx, users); n != 1 {
		t.Fatalf("live activity deleted: %d", n)
	}
}
