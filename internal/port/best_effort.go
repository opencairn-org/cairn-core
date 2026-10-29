package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// BestEffortRepo persists computed best efforts. The unique constraint
// in migration 8 means re-running the compute for the same source
// produces an UPSERT-or-replace pattern: callers typically delete by
// source first, then insert the new set within one transaction.
type BestEffortRepo interface {
	// DeleteForSource removes all best_efforts rows for the source.
	// Used at the start of recompute to give a clean slate.
	DeleteForSource(ctx context.Context, sourceID domain.SourceID) error

	// SaveEfforts inserts new best_efforts rows. Caller is expected to
	// have called DeleteForSource first; this method itself uses INSERT
	// not UPSERT and the UNIQUE constraint will trip on duplicates.
	SaveEfforts(ctx context.Context, efforts []domain.BestEffort) error

	// ListForActivity returns every best_effort for the activity (across
	// all of its sources), ordered metric → window_value for chart
	// rendering.
	ListForActivity(ctx context.Context, activityID domain.ActivityID) ([]domain.BestEffort, error)

	// ListPersonalRecords returns the user's all-time best per (activity type,
	// metric, window) across every activity and provider — the cross-provider PR
	// table (brief §11). Computed on read, so it's always current.
	ListPersonalRecords(ctx context.Context, userID domain.UserID) ([]domain.PersonalRecord, error)

	// ListBestEffortHistory returns every effort of the user in one bucket
	// (activity type, metric, window), chronological, for the progression view.
	ListBestEffortHistory(ctx context.Context, userID domain.UserID, activityType, metric, windowKind string, windowValue int) ([]domain.BestEffortHistoryItem, error)

	// DeleteForUserWindow removes every effort of the user in one (activity
	// type, metric, window) bucket — used when a custom window is deleted.
	DeleteForUserWindow(ctx context.Context, userID domain.UserID, activityType domain.ActivityType, metric domain.BestEffortMetric, kind domain.BestEffortWindowKind, value int) error
}

// CustomBestEffortRepo persists user-defined best-effort windows.
type CustomBestEffortRepo interface {
	// Create inserts a window; a duplicate (user, type, metric, kind, value)
	// returns domain.ErrCustomEffortDuplicate.
	Create(ctx context.Context, c domain.CustomBestEffort) (domain.CustomBestEffortID, error)
	// ListForUser returns the user's windows, oldest first; activityType ""
	// means every type.
	ListForUser(ctx context.Context, userID domain.UserID, activityType domain.ActivityType) ([]domain.CustomBestEffort, error)
	// Delete removes one window (ownership-scoped); ErrNotFound when absent.
	Delete(ctx context.Context, userID domain.UserID, id domain.CustomBestEffortID) error
}
