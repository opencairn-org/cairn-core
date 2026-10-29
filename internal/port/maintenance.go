package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// MaintenanceRepo is the bulk-iteration surface behind the maintenance
// operations. Every list is keyset-paged by id ascending and limited to the
// given users when the slice is non-empty.
type MaintenanceRepo interface {
	// Live activities (not soft-deleted) and their active sources.
	CountActivities(ctx context.Context, users []domain.UserID) (int64, error)
	ListActivityIDs(ctx context.Context, users []domain.UserID, after domain.ActivityID, limit int) ([]domain.ActivityID, error)
	CountSources(ctx context.Context, users []domain.UserID) (int64, error)
	ListSourceIDs(ctx context.Context, users []domain.UserID, after domain.SourceID, limit int) ([]domain.SourceID, error)
	// Active users.
	CountUsers(ctx context.Context, users []domain.UserID) (int64, error)
	ListUserIDs(ctx context.Context, users []domain.UserID, after domain.UserID, limit int) ([]domain.UserID, error)
	// Activities soft-deleted before the cut-off.
	CountSoftDeletedActivities(ctx context.Context, users []domain.UserID, before time.Time) (int64, error)
	ListSoftDeletedActivityIDs(ctx context.Context, users []domain.UserID, before time.Time, after domain.ActivityID, limit int) ([]domain.ActivityID, error)
	// HardDeleteActivity removes the row (sources and attachments cascade) and
	// returns the blob keys that were referenced only by it.
	HardDeleteActivity(ctx context.Context, id domain.ActivityID) (blobKeys []string, err error)
	// ReferencedBlobKeys reports which of keys a source or attachment still references.
	ReferencedBlobKeys(ctx context.Context, keys []string) (map[string]bool, error)
}

// BlobLister enumerates stored objects; optional on a BlobStore.
type BlobLister interface {
	ListBlobKeys(ctx context.Context, prefix string, visit func(key string, modified time.Time) error) error
}

// MaintenanceJobRepo keeps the job history across restarts; the runner
// writes through on every state change.
type MaintenanceJobRepo interface {
	// SaveJob inserts or rewrites the row.
	SaveJob(ctx context.Context, job domain.MaintenanceJob) error
	// GetJob reads one row; domain.ErrNotFound on miss.
	GetJob(ctx context.Context, id string) (domain.MaintenanceJob, error)
	// ListJobs returns the newest rows first.
	ListJobs(ctx context.Context, limit int) ([]domain.MaintenanceJob, error)
	// ListOpenJobs returns queued and running rows, oldest first.
	ListOpenJobs(ctx context.Context) ([]domain.MaintenanceJob, error)
}
