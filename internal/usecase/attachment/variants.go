// Package attachment holds the photo use cases shared by the upload handler,
// the ingest mirror and the backfill command.
package attachment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/imaging"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ErrNotImage: the blob is not a decodable image, so no variants exist.
var ErrNotImage = errors.New("attachment: not a decodable image")

// Variants derives and stores the display variants of attachments.
type Variants struct {
	attachments port.AttachmentRepo
	blobs       port.BlobStore
	logger      *slog.Logger
}

// NewVariants wires the use case; blobs may be nil (then nothing is derived).
func NewVariants(attachments port.AttachmentRepo, blobs port.BlobStore, logger *slog.Logger) *Variants {
	if logger == nil {
		logger = slog.Default()
	}
	return &Variants{attachments: attachments, blobs: blobs, logger: logger}
}

// Available reports whether variants can be derived (blob storage wired).
func (v *Variants) Available() bool { return v != nil && v.blobs != nil }

// Derive generates the variants from raw and stores the JPEGs next to the
// original (<key>.thumb.jpg / <key>.medium.jpg). The returned attachment
// carries the keys, blurhash and size; the caller persists it.
func (v *Variants) Derive(ctx context.Context, a domain.Attachment, raw []byte) (domain.Attachment, error) {
	if !v.Available() {
		return a, nil
	}
	vs, err := imaging.Derive(raw)
	if err != nil {
		return a, fmt.Errorf("%w: %w", ErrNotImage, err)
	}
	thumbKey := a.BlobID + ".thumb.jpg"
	if err := v.blobs.Put(ctx, thumbKey, vs.Thumb, "image/jpeg"); err != nil {
		return a, fmt.Errorf("store thumb: %w", err)
	}
	mediumKey := ""
	if vs.Medium != nil {
		mediumKey = a.BlobID + ".medium.jpg"
		if err := v.blobs.Put(ctx, mediumKey, vs.Medium, "image/jpeg"); err != nil {
			return a, fmt.Errorf("store medium: %w", err)
		}
	}
	a.ThumbBlobID, a.MediumBlobID, a.Blurhash = thumbKey, mediumKey, vs.Blurhash
	a.Width, a.Height = vs.Width, vs.Height
	return a, nil
}

// Ensure derives and records the variants of a stored attachment that lacks
// them (worker-mirrored photos, pre-variant rows). A no-op when present.
func (v *Variants) Ensure(ctx context.Context, a domain.Attachment) error {
	if a.HasVariants() || !v.Available() {
		return nil
	}
	raw, _, err := v.blobs.Get(ctx, a.BlobID)
	if err != nil {
		return fmt.Errorf("load original %s: %w", a.BlobID, err)
	}
	derived, err := v.Derive(ctx, a, raw)
	if err != nil {
		return err
	}
	return v.attachments.SetVariants(ctx, a.ID, derived.ThumbBlobID, derived.MediumBlobID, derived.Blurhash, derived.Width, derived.Height)
}

// EnsureForActivity runs Ensure over every attachment of an activity,
// logging and continuing on failure (best-effort follow-up).
func (v *Variants) EnsureForActivity(ctx context.Context, activityID domain.ActivityID) {
	if !v.Available() {
		return
	}
	atts, err := v.attachments.ListForActivity(ctx, activityID)
	if err != nil {
		v.logger.Warn("attachment variants: list failed", "activity_id", activityID, "error", err)
		return
	}
	for _, a := range atts {
		if err := v.Ensure(ctx, a); err != nil {
			v.logger.Warn("attachment variants: derive failed", "attachment_id", a.ID, "error", err)
		}
	}
}

// BackfillResult counts one Backfill run.
type BackfillResult struct {
	Done, Failed int
}

// Backfill derives variants for every attachment that lacks them, in batches,
// until none are left or ctx ends. Failures are logged and counted; a failing
// row is retried on the next run (it still lacks variants), so a persistent
// failure keeps surfacing instead of being hidden.
func (v *Variants) Backfill(ctx context.Context, batch int) (BackfillResult, error) {
	var res BackfillResult
	if !v.Available() {
		return res, errors.New("attachment: blob storage is not configured")
	}
	if batch <= 0 {
		batch = 100
	}
	seen := map[domain.AttachmentID]bool{}
	for {
		atts, err := v.attachments.ListWithoutVariants(ctx, batch)
		if err != nil {
			return res, err
		}
		if !v.backfillBatch(ctx, atts, seen, &res) || ctx.Err() != nil {
			return res, ctx.Err()
		}
	}
}

// backfillBatch processes one page; false when nothing new was attempted.
func (v *Variants) backfillBatch(ctx context.Context, atts []domain.Attachment, seen map[domain.AttachmentID]bool, res *BackfillResult) bool {
	progress := false
	for _, a := range atts {
		if seen[a.ID] {
			continue // failed before in this run; do not loop on it
		}
		seen[a.ID] = true
		progress = true
		if err := v.Ensure(ctx, a); err != nil {
			res.Failed++
			v.logger.Warn("attachment variants: backfill failed", "attachment_id", a.ID, "blob", a.BlobID, "error", err)
			continue
		}
		res.Done++
	}
	return progress
}

// IsImageContentType reports whether ct names an image.
func IsImageContentType(ct string) bool { return strings.HasPrefix(ct, "image/") }
