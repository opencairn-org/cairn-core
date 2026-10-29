//go:build integration

package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestSourcePreviewRepo(t *testing.T) {
	ctx, pool := requirePool(t)
	users := NewUserRepo(pool)
	uid := domain.UserID(uuid.New())
	suffix := uid.String()[:8]
	if err := users.CreateUser(ctx, domain.User{
		ID: uid, Username: "prev-" + suffix, Email: "prev-" + suffix + "@example.com",
		DisplayName: "Prev", Role: domain.UserRoleUser, Status: domain.UserStatusActive, Units: domain.UserUnitsMetric,
	}, domain.Credentials{UserID: uid}); err != nil {
		t.Fatal(err)
	}
	acts := NewActivityRepo(pool)
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
		ID: srcID, ActivityID: actID, UserID: uid, Provider: "upload", ExternalID: "x-" + suffix,
		SourceWorkerName: "w", SourceWorkerVersion: "1.0", SourceWorkerPackage: "p", Status: domain.SourceStatusActive,
		ReimportStatus: domain.ReimportStatusCurrent, ImportedAt: now, UpdatedAt: now,
		Parsed: domain.ActivitySourcePayload{Type: domain.ActivityTypeRide, StartTime: now, EndTime: now.Add(time.Hour), ElapsedDuration: time.Hour, MovingDuration: time.Hour, Timezone: "UTC"},
	}); err != nil {
		t.Fatalf("save source: %v", err)
	}

	repo := NewSourcePreviewRepo(pool)
	id, err := repo.Create(ctx, domain.SourcePreview{SourceID: srcID, UserID: uid, JobID: "job-1", ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := repo.FindPendingForSource(ctx, srcID, now)
	if err != nil || pending.ID != id || pending.ActivityID != actID || pending.Status != domain.SourcePreviewPending {
		t.Fatalf("pending: %v %+v", err, pending)
	}
	if err := repo.MarkReady(ctx, id, []byte(`{"a":1}`), []domain.FieldChange{{Field: "title", Before: "a", After: "b"}}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, id)
	if err != nil || got.Status != domain.SourcePreviewReady || len(got.SourceDiff) != 1 || len(got.MergedDiff) != 0 || string(got.Parsed) != `{"a": 1}` {
		t.Fatalf("ready: %v %+v %s", err, got, got.Parsed)
	}
	if _, err := repo.FindPendingForSource(ctx, srcID, now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no longer pending: %v", err)
	}
	if err := repo.Delete(ctx, domain.UserID(uuid.New()), id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign delete: %v", err)
	}
	if err := repo.Delete(ctx, uid, id); err != nil {
		t.Fatal(err)
	}
	old, _ := repo.Create(ctx, domain.SourcePreview{SourceID: srcID, UserID: uid, ExpiresAt: now.Add(-time.Minute)})
	if n, err := repo.DeleteExpired(ctx, now); err != nil || n < 1 {
		t.Fatalf("expired: %d %v", n, err)
	}
	if _, err := repo.Get(ctx, old); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired row gone: %v", err)
	}
}
