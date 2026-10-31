package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ImportQueueFilter narrows the admin listing; zero values mean "any".
type ImportQueueFilter struct {
	Statuses      []domain.ImportItemStatus
	ItemTypes     []domain.ImportItemType
	Providers     []string
	UserID        *domain.UserID
	From, To      time.Time // created_at range, [From, To)
	Limit, Offset int
}

// ImportQueueProcessor drives the queue: enqueue, claim, complete and recover items.
type ImportQueueProcessor interface {
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

// ImportQueueAccountView serves an account's queue view and its per-item actions.
type ImportQueueAccountView interface {
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
}

// ImportQueueAdmin serves the instance-wide admin queue view.
type ImportQueueAdmin interface {
	// ListAll pages items across every account with the filter applied,
	// in-progress first then newest; the total ignores the page.
	ListAll(ctx context.Context, f ImportQueueFilter) ([]domain.ImportQueueItem, int, error)

	// Get returns one item by id; domain.ErrNotFound when absent.
	Get(ctx context.Context, id domain.ImportQueueItemID) (domain.ImportQueueItem, error)

	// CountAllByStatus counts items per status across the instance.
	CountAllByStatus(ctx context.Context, f ImportQueueFilter) (map[domain.ImportItemStatus]int, error)

	// Retry puts a failed or skipped item back to pending; reset clears attempts.
	Retry(ctx context.Context, id domain.ImportQueueItemID, reset bool) (bool, error)

	// Cancel marks a pending or in-progress item skipped.
	Cancel(ctx context.Context, id domain.ImportQueueItemID, reason string) (bool, error)
}

// ImportQueueRepo persists the core-driven import work list. See migration
// 00017 and docs in domain.ImportQueueItem.
type ImportQueueRepo interface {
	ImportQueueProcessor
	ImportQueueAccountView
	ImportQueueAdmin
}
