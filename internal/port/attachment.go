package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// AttachmentRepo persists activity attachments (photos/media). Bytes live in
// the BlobStore; this stores the metadata + blob key.
type AttachmentRepo interface {
	// ListForActivity returns the activity's attachments ordered by position
	// then created_at.
	ListForActivity(ctx context.Context, activityID domain.ActivityID) ([]domain.Attachment, error)

	// Get returns one attachment by ID (for ownership checks + raw serving).
	Get(ctx context.Context, id domain.AttachmentID) (domain.Attachment, error)

	// Add inserts one attachment.
	Add(ctx context.Context, a domain.Attachment) error

	// ReplaceForSource atomically replaces all attachments that came from a
	// given source (reimport-safe). attachments may be empty (clears them).
	ReplaceForSource(ctx context.Context, activityID domain.ActivityID, sourceID domain.SourceID, attachments []domain.Attachment) error

	// Delete removes one attachment. Returns the deleted row's BlobID so the
	// caller can drop the blob. Returns domain.ErrNotFound if absent.
	Delete(ctx context.Context, id domain.AttachmentID) (blobID string, err error)

	// SetVariants records the derived variant keys, blurhash and the
	// original's pixel size.
	SetVariants(ctx context.Context, id domain.AttachmentID, thumbBlobID, mediumBlobID, blurhash string, width, height int) error

	// ListWithoutVariants pages attachments that still lack variants, for the
	// backfill command; ordered by created_at.
	ListWithoutVariants(ctx context.Context, limit int) ([]domain.Attachment, error)
}
