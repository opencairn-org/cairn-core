package attachment

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Photo errors, mapped to HTTP by the adapters.
var (
	ErrNoStorage   = port.NewActionError(port.ActionErrorUnavailable, "storage not configured")
	ErrBlobMissing = port.NewActionError(port.ActionErrorUpstream, "blob unavailable")
	ErrStore       = port.NewActionError(port.ActionErrorInternal, "store failed")
	ErrSave        = port.NewActionError(port.ActionErrorInternal, "save failed")
	ErrDelete      = port.NewActionError(port.ActionErrorInternal, "delete failed")
)

// Photos are an activity's attachments as the reader and the owner see them.
// Callers have already decided who may see or change the activity.
type Photos struct {
	attachments port.AttachmentRepo
	blobs       port.BlobStore // nil when storage is not configured
	variants    *Variants
	content     port.ContentSigner // nil: no signed URLs
	logger      *slog.Logger
}

// NewPhotos wires the use case; blobs, variants and content may be nil.
func NewPhotos(attachments port.AttachmentRepo, blobs port.BlobStore, variants *Variants, content port.ContentSigner, logger *slog.Logger) *Photos {
	if logger == nil {
		logger = slog.Default()
	}
	return &Photos{attachments: attachments, blobs: blobs, variants: variants, content: content, logger: logger}
}

// HasStorage reports whether photo bytes can be read or written.
func (p *Photos) HasStorage() bool { return p.blobs != nil }

// List returns the activity's attachments.
func (p *Photos) List(ctx context.Context, activityID domain.ActivityID) ([]domain.Attachment, error) {
	return p.attachments.ListForActivity(ctx, activityID)
}

// Get is attachment id when it belongs to activityID.
func (p *Photos) Get(ctx context.Context, activityID domain.ActivityID, id domain.AttachmentID) (domain.Attachment, error) {
	att, err := p.attachments.Get(ctx, id)
	if err != nil || att.ActivityID != activityID {
		return domain.Attachment{}, domain.ErrNotFound
	}
	return att, nil
}

// SignedURL is one signed variant URL.
type SignedURL struct {
	URL       string
	ExpiresAt time.Time
}

// URLs signs the derived variants that exist, plus the original (which keeps
// its EXIF) for the owner; nil without a signer.
func (p *Photos) URLs(ctx context.Context, a domain.Attachment, isOwner bool) map[string]SignedURL {
	if p.content == nil {
		return nil
	}
	original := ""
	if isOwner {
		original = a.BlobID
	}
	out := map[string]SignedURL{}
	for name, key := range map[string]string{"original": original, "medium": a.MediumBlobID, "thumb": a.ThumbBlobID} {
		if key == "" {
			continue
		}
		ct := "image/jpeg"
		if name == "original" {
			ct = a.ContentType
		}
		if u, err := p.content.Blob(ctx, key, ct, ""); err == nil {
			out[name] = SignedURL{URL: u.URL, ExpiresAt: u.ExpiresAt}
		}
	}
	return out
}

// Bytes reads the photo for a viewer. Non-owners only get a re-encoded
// variant: the original keeps EXIF GPS.
func (p *Photos) Bytes(ctx context.Context, a domain.Attachment, isOwner bool) ([]byte, string, error) {
	if p.blobs == nil {
		return nil, "", ErrNoStorage
	}
	key, wantCT := a.BlobID, a.ContentType
	if !isOwner {
		key, wantCT = a.DisplayBlobID(), "image/jpeg"
		if key == "" {
			return nil, "", domain.ErrNotFound
		}
	}
	data, ct, err := p.blobs.Get(ctx, key)
	if err != nil {
		return nil, "", ErrBlobMissing
	}
	if wantCT != "" {
		ct = wantCT
	}
	return data, ct, nil
}

// Upload stores an owner's photo with its display variants.
func (p *Photos) Upload(ctx context.Context, owner domain.UserID, activityID domain.ActivityID, raw []byte, contentType, caption string) error {
	if p.blobs == nil {
		return ErrNoStorage
	}
	key := "attachments/" + owner.String() + "/" + activityID.String() + "/" + uuid.NewString()
	if err := p.blobs.Put(ctx, key, raw, contentType); err != nil {
		p.logger.Error("attachment upload: blob put", "error", err)
		return ErrStore
	}
	att := domain.Attachment{
		ActivityID:  activityID,
		UserID:      owner,
		BlobID:      key,
		ContentType: contentType,
		Caption:     strings.TrimSpace(caption),
	}
	// Variants are best-effort: an undecodable image is still stored as is.
	if derived, err := p.variants.Derive(ctx, att, raw); err == nil {
		att = derived
	} else {
		p.logger.Warn("attachment upload: variants failed", "key", key, "error", err)
	}
	if err := p.attachments.Add(ctx, att); err != nil {
		return ErrSave
	}
	return nil
}

// Remove deletes one photo of the activity and drops its blobs best-effort.
func (p *Photos) Remove(ctx context.Context, activityID domain.ActivityID, id domain.AttachmentID) error {
	att, err := p.Get(ctx, activityID, id)
	if err != nil {
		return err
	}
	blobID, err := p.attachments.Delete(ctx, id)
	if err != nil {
		return ErrDelete
	}
	if p.blobs == nil {
		return nil
	}
	for _, key := range []string{blobID, att.ThumbBlobID, att.MediumBlobID} {
		if key == "" {
			continue
		}
		if err := p.blobs.Delete(ctx, key); err != nil {
			p.logger.Warn("attachment delete: blob drop failed", "blob_id", key, "error", err)
		}
	}
	return nil
}
