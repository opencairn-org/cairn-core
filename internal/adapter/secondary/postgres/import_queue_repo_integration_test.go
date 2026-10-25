//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// queueFixture creates a user + external account to hang queue rows off, and
// returns the queue repo scoped to that account.
func queueFixture(t *testing.T) (context.Context, *ImportQueueRepo, domain.ExternalAccount) {
	t.Helper()
	ctx, pool := requirePool(t)

	userID := domain.UserID(uuid.New())
	suffix := userID.String()[:8]
	if err := NewUserRepo(pool).CreateUser(ctx, domain.User{
		ID:          userID,
		Username:    "qtest-" + suffix,
		Email:       "qtest-" + suffix + "@example.com",
		DisplayName: "Queue Test",
		Role:        domain.UserRoleUser,
		Status:      domain.UserStatusActive,
		Units:       domain.UserUnitsMetric,
	}, domain.Credentials{UserID: userID, PasswordHash: "x"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	acctID, err := NewExternalAccountRepo(pool, 0, 0).CreateAccount(ctx, domain.ExternalAccount{
		UserID:            userID,
		Provider:          "strava",
		ProviderAccountID: "qtest-" + suffix,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return ctx, NewImportQueueRepo(pool), domain.ExternalAccount{ID: acctID, UserID: userID, Provider: "strava"}
}

func enqueueActivities(t *testing.T, ctx context.Context, repo *ImportQueueRepo, a domain.ExternalAccount, extIDs []string, times []time.Time) map[string]domain.ImportQueueItem {
	t.Helper()
	items := make([]domain.ImportQueueItem, len(extIDs))
	for i, ext := range extIDs {
		items[i] = domain.ImportQueueItem{
			ExternalAccountID: a.ID,
			UserID:            a.UserID,
			Provider:          a.Provider,
			ItemType:          domain.ImportItemActivity,
			ExternalID:        ext,
			ItemTime:          &times[i],
		}
	}
	if n, err := repo.Enqueue(ctx, items); err != nil || n != len(items) {
		t.Fatalf("Enqueue: n=%d err=%v", n, err)
	}
	listed, err := repo.ListForAccount(ctx, a.ID, nil, 100)
	if err != nil {
		t.Fatalf("ListForAccount: %v", err)
	}
	byExt := map[string]domain.ImportQueueItem{}
	for _, it := range listed {
		byExt[it.ExternalID] = it
	}
	return byExt
}

// TestImportQueueRepo_MoveToTop proves the manual priority bump wins over the
// default newest-first claim order, that repeated bumps stack (last bump goes
// first), and that only pending rows can be bumped.
func TestImportQueueRepo_MoveToTop(t *testing.T) {
	ctx, repo, acct := queueFixture(t)

	base := time.Now().UTC().Truncate(time.Second)
	byExt := enqueueActivities(t, ctx, repo, acct,
		[]string{"oldest", "middle", "newest"},
		[]time.Time{base.Add(-3 * time.Hour), base.Add(-2 * time.Hour), base.Add(-1 * time.Hour)})

	// Bump the oldest, then the middle: claim order must become
	// middle (bumped last), oldest (bumped first), newest (priority 0).
	for _, ext := range []string{"oldest", "middle"} {
		ok, err := repo.MoveToTop(ctx, acct.ID, byExt[ext].ID)
		if err != nil || !ok {
			t.Fatalf("MoveToTop(%s): ok=%v err=%v", ext, ok, err)
		}
	}

	listed, err := repo.ListForAccount(ctx, acct.ID, nil, 100)
	if err != nil {
		t.Fatalf("ListForAccount: %v", err)
	}
	if listed[0].ExternalID != "middle" || listed[0].Priority == 0 {
		t.Fatalf("list head = %q (priority %d), want bumped item first", listed[0].ExternalID, listed[0].Priority)
	}

	claimed, err := repo.ClaimPending(ctx, acct.ID, 3)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	var order []string
	for _, it := range claimed {
		order = append(order, it.ExternalID)
	}
	want := []string{"middle", "oldest", "newest"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("claim order = %v, want %v", order, want)
		}
	}

	// All claimed rows are in_progress now — bumping must be a no-op.
	if ok, err := repo.MoveToTop(ctx, acct.ID, byExt["newest"].ID); err != nil || ok {
		t.Fatalf("MoveToTop on in_progress: ok=%v err=%v, want false", ok, err)
	}
}

// TestImportQueueRepo_RequeueFailed proves a failed row goes back to a clean
// pending state (attempts, error and timestamps reset) and that the method
// refuses rows in any other state.
func TestImportQueueRepo_RequeueFailed(t *testing.T) {
	ctx, repo, acct := queueFixture(t)

	when := time.Now().UTC().Truncate(time.Second)
	byExt := enqueueActivities(t, ctx, repo, acct, []string{"a1"}, []time.Time{when})
	id := byExt["a1"].ID

	// Pending rows can't be "requeued" — nothing to retry yet.
	if ok, err := repo.RequeueFailed(ctx, acct.ID, id); err != nil || ok {
		t.Fatalf("RequeueFailed on pending: ok=%v err=%v, want false", ok, err)
	}

	if _, err := repo.ClaimPending(ctx, acct.ID, 1); err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if err := repo.MarkFailed(ctx, id, "worker exploded"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	ok, err := repo.RequeueFailed(ctx, acct.ID, id)
	if err != nil || !ok {
		t.Fatalf("RequeueFailed: ok=%v err=%v", ok, err)
	}

	listed, err := repo.ListForAccount(ctx, acct.ID, []domain.ImportItemStatus{domain.ImportStatusPending}, 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListForAccount after requeue: n=%d err=%v", len(listed), err)
	}
	got := listed[0]
	if got.Attempts != 0 || got.LastError != "" || got.StartedAt != nil || got.CompletedAt != nil {
		t.Fatalf("requeued row not reset: attempts=%d lastError=%q startedAt=%v completedAt=%v",
			got.Attempts, got.LastError, got.StartedAt, got.CompletedAt)
	}

	// The requeued row is claimable again and counts attempts from scratch.
	claimed, err := repo.ClaimPending(ctx, acct.ID, 1)
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 1 {
		t.Fatalf("re-claim after requeue: %+v err=%v", claimed, err)
	}
}

// TestImportQueueRepo_Requeue proves a re-download resets rows an earlier
// sync finished (done, failed) to a fresh pending attempt, inserts unknown
// ids, and leaves pending/in_progress rows alone.
func TestImportQueueRepo_Requeue(t *testing.T) {
	ctx, repo, acct := queueFixture(t)

	when := time.Now().UTC().Truncate(time.Second)
	byExt := enqueueActivities(t, ctx, repo, acct, []string{"done", "failed", "pending", "running"},
		[]time.Time{when, when.Add(time.Second), when.Add(2 * time.Second), when.Add(3 * time.Second)})

	// Claim all four, then settle "done" and "failed"; "running" stays
	// in_progress and "pending" goes back to pending via requeue-failed.
	if _, err := repo.ClaimPending(ctx, acct.ID, 4); err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	if err := repo.MarkDone(ctx, byExt["done"].ID); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if err := repo.MarkFailed(ctx, byExt["failed"].ID, "boom"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if err := repo.MarkFailed(ctx, byExt["pending"].ID, "boom"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if ok, err := repo.RequeueFailed(ctx, acct.ID, byExt["pending"].ID); err != nil || !ok {
		t.Fatalf("RequeueFailed: ok=%v err=%v", ok, err)
	}

	// Plain Enqueue of the same ids is a no-op: that is the bug being fixed.
	same := func(ext string) domain.ImportQueueItem {
		return domain.ImportQueueItem{ExternalAccountID: acct.ID, UserID: acct.UserID, Provider: acct.Provider,
			ItemType: domain.ImportItemActivity, ExternalID: ext, ItemTime: &when}
	}
	items := []domain.ImportQueueItem{same("done"), same("failed"), same("pending"), same("running"), same("new")}
	if n, err := repo.Enqueue(ctx, items); err != nil || n != 1 {
		t.Fatalf("Enqueue: n=%d err=%v, want 1 (only the unknown id)", n, err)
	}

	// Requeue: "done" + "failed" reset, "new" already inserted above, the
	// pending and in_progress rows untouched → 2 rows affected.
	n, err := repo.Requeue(ctx, items)
	if err != nil || n != 2 {
		t.Fatalf("Requeue: n=%d err=%v, want 2", n, err)
	}

	listed, err := repo.ListForAccount(ctx, acct.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListForAccount: %v", err)
	}
	status := map[string]domain.ImportQueueItem{}
	for _, it := range listed {
		status[it.ExternalID] = it
	}
	for _, ext := range []string{"done", "failed", "pending", "new"} {
		got := status[ext]
		if got.Status != domain.ImportStatusPending || got.Attempts != 0 || got.LastError != "" || got.CompletedAt != nil {
			t.Fatalf("%s after requeue: %+v, want fresh pending", ext, got)
		}
	}
	if got := status["running"]; got.Status != domain.ImportStatusInProgress || got.Attempts != 1 {
		t.Fatalf("running after requeue: %+v, want untouched in_progress", got)
	}
}
