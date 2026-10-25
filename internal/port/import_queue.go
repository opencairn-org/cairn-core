package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ImportQueueRepo persists the core-driven import work list. See migration
// 00017 and docs in domain.ImportQueueItem.
type ImportQueueRepo interface {
	// Enqueue bulk-inserts pending items, skipping ones already queued for
	// the same (account, type, external_id) — ON CONFLICT DO NOTHING.
	// Returns the number of rows newly inserted.
	Enqueue(ctx context.Context, items []domain.ImportQueueItem) (int, error)

	// Requeue is Enqueue for a re-download: items whose row is already
	// done, failed or skipped go back to pending; pending and in_progress
	// rows are left alone. Returns the number of rows inserted or reset.
	Requeue(ctx context.Context, items []domain.ImportQueueItem) (int, error)

	// ClaimPending atomically marks up to limit pending items for an account
	// as in_progress (FOR UPDATE SKIP LOCKED) and returns them. Safe to call
	// from multiple processor instances.
	ClaimPending(ctx context.Context, accountID domain.ExternalAccountID, limit int) ([]domain.ImportQueueItem, error)

	// MarkDone / MarkFailed transition a claimed (in_progress) item.
	MarkDone(ctx context.Context, id domain.ImportQueueItemID) error
	MarkFailed(ctx context.Context, id domain.ImportQueueItemID, reason string) error

	// MarkDoneByExternalID marks the in_progress item for (account, type,
	// external_id) done. The result router calls this after a successful
	// ingest — correlating by the dedup key instead of threading a job id.
	MarkDoneByExternalID(ctx context.Context, accountID domain.ExternalAccountID, itemType domain.ImportItemType, externalID string) error

	// MarkFailedByExternalID fails the in_progress item for (account, type,
	// externalID) — used when ingest rejects the payload terminally so the
	// capacity-gated queue doesn't deadlock on a permanently-bad item.
	MarkFailedByExternalID(ctx context.Context, accountID domain.ExternalAccountID, itemType domain.ImportItemType, externalID, reason string) error

	// CountByStatus returns per-status counts for an account — the UI queue
	// view ("142 pending, 8 failed").
	CountByStatus(ctx context.Context, accountID domain.ExternalAccountID) (map[domain.ImportItemStatus]int, error)

	// ListForAccount returns the account's queue items filtered by status
	// (empty = all), in_progress first then newest item_time — the debugging
	// queue view on the connections page.
	ListForAccount(ctx context.Context, accountID domain.ExternalAccountID, statuses []domain.ImportItemStatus, limit int) ([]domain.ImportQueueItem, error)

	// RequeueFailed flips one failed item back to pending — attempts, error
	// and completion timestamps reset — so the processor picks it up again.
	// Account-scoped so a handler cannot touch another user's rows. Returns
	// false when the item doesn't exist or isn't failed.
	RequeueFailed(ctx context.Context, accountID domain.ExternalAccountID, id domain.ImportQueueItemID) (bool, error)

	// MoveToTop bumps one pending item's priority above the account's current
	// pending maximum so it is claimed next. Returns false when the item
	// doesn't exist or isn't pending.
	MoveToTop(ctx context.Context, accountID domain.ExternalAccountID, id domain.ImportQueueItemID) (bool, error)

	// RequeueStale flips items that have sat in_progress longer than olderThan
	// back to pending so they get re-dispatched; items already at maxAttempts
	// are failed instead. Guards against dispatched jobs whose result never
	// arrives (worker Term/DLQ, lost message) deadlocking a queue slot forever.
	// Returns (requeued, failed).
	RequeueStale(ctx context.Context, olderThan time.Duration, maxAttempts int) (int, int, error)

	// AccountsWithPending lists external_account ids that currently have
	// pending items — the processor's scan target.
	AccountsWithPending(ctx context.Context, limit int) ([]domain.ExternalAccountID, error)
}
