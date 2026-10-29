package port

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// SourcePreviewRepo persists reimport previews (activity_source_previews).
type SourcePreviewRepo interface {
	// Create inserts a pending preview and returns its id.
	Create(ctx context.Context, p domain.SourcePreview) (domain.SourcePreviewID, error)
	// Get returns one preview with its activity id resolved; ErrNotFound when absent.
	Get(ctx context.Context, id domain.SourcePreviewID) (domain.SourcePreview, error)
	// FindPendingForSource returns the oldest unexpired pending preview of a
	// source, or ErrNotFound.
	FindPendingForSource(ctx context.Context, sourceID domain.SourceID, now time.Time) (domain.SourcePreview, error)
	// MarkReady stores the proposed payload and diffs and flips the status.
	MarkReady(ctx context.Context, id domain.SourcePreviewID, parsed []byte, sourceDiff, mergedDiff []domain.FieldChange) error
	// Delete removes one preview (ownership-scoped); ErrNotFound when absent.
	Delete(ctx context.Context, userID domain.UserID, id domain.SourcePreviewID) error
	// DeleteExpired drops every preview past its expiry.
	DeleteExpired(ctx context.Context, now time.Time) (int, error)
}
