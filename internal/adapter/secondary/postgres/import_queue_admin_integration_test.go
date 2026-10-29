//go:build integration

package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

func TestImportQueueRepo_AdminViews(t *testing.T) {
	ctx, repo, acct := queueFixture(t)
	now := time.Now().UTC()
	items := enqueueActivities(t, ctx, repo, acct, []string{"adm-a", "adm-b"}, []time.Time{now, now.Add(-time.Hour)})
	a, b := items["adm-a"], items["adm-b"]
	if err := repo.MarkFailed(ctx, a.ID, "boom"); err != nil {
		t.Fatal(err)
	}

	uid := acct.UserID
	list, total, err := repo.ListAll(ctx, port.ImportQueueFilter{UserID: &uid})
	if err != nil || total != 2 || len(list) != 2 {
		t.Fatalf("list all: %v total=%d n=%d", err, total, len(list))
	}
	failed, total, _ := repo.ListAll(ctx, port.ImportQueueFilter{UserID: &uid, Statuses: []domain.ImportItemStatus{domain.ImportStatusFailed}})
	if total != 1 || len(failed) != 1 || failed[0].ID != a.ID || failed[0].LastError != "boom" {
		t.Fatalf("failed filter: %d %+v", total, failed)
	}
	counts, err := repo.CountAllByStatus(ctx, port.ImportQueueFilter{UserID: &uid})
	if err != nil || counts[domain.ImportStatusFailed] != 1 || counts[domain.ImportStatusPending] != 1 {
		t.Fatalf("counts: %v %v", err, counts)
	}
	got, err := repo.Get(ctx, a.ID)
	if err != nil || got.ExternalID != "adm-a" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := repo.Get(ctx, domain.ImportQueueItemID(uuid.New())); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if ok, err := repo.Retry(ctx, b.ID, true); err != nil || ok {
		t.Fatalf("retry pending must be a no-op: %v %v", ok, err)
	}
	if ok, err := repo.Retry(ctx, a.ID, true); err != nil || !ok {
		t.Fatalf("retry failed: %v %v", ok, err)
	}
	if got, _ := repo.Get(ctx, a.ID); got.Status != domain.ImportStatusPending || got.Attempts != 0 || got.LastError != "" {
		t.Fatalf("after retry: %+v", got)
	}
	if ok, err := repo.Cancel(ctx, a.ID, "cancelled by admin"); err != nil || !ok {
		t.Fatalf("cancel: %v %v", ok, err)
	}
	if got, _ := repo.Get(ctx, a.ID); got.Status != domain.ImportStatusSkipped {
		t.Fatalf("after cancel: %+v", got)
	}
	if groups, err := NewActivityRepo(repo.pool).SourceWorkerGroups(ctx); err != nil || groups == nil {
		t.Fatalf("source worker groups: %v", err)
	}
	if n, err := NewDeadLetterRepo(repo.pool).CountOpen(ctx); err != nil || n < 0 {
		t.Fatalf("count open: %d %v", n, err)
	}
}
