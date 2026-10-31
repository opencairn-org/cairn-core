package domain

import "time"

// Attachment is a photo/media item belonging to an activity — a first-class
// entity, not a merged field. Bytes live in blob storage (BlobID, an S3 key);
// ExternalURL records the original provider URL for provenance. Provider-
// imported attachments carry the SourceID they came from (so a reimport can
// replace exactly that source's set); user uploads have SourceID == nil.
type Attachment struct {
	ID          AttachmentID
	ActivityID  ActivityID
	SourceID    *SourceID
	UserID      UserID
	BlobID      string
	ExternalURL string
	ContentType string
	Caption     string
	Width       int
	Height      int
	Position    int
	CreatedAt   time.Time

	// Derived display variants (JPEG keys in blob storage) and a blurhash
	// placeholder; empty until generated. The variants are re-encoded, so they
	// carry no EXIF (GPS); the original keeps it and is owner-only.
	ThumbBlobID  string
	MediumBlobID string
	Blurhash     string
}

// HasVariants reports whether the display variants were generated.
func (a Attachment) HasVariants() bool {
	return a.ThumbBlobID != "" && a.MediumBlobID != "" && a.Blurhash != ""
}

// DisplayBlobID is the largest metadata-free variant, for non-owner reads;
// empty until variants exist.
func (a Attachment) DisplayBlobID() string {
	if a.MediumBlobID != "" {
		return a.MediumBlobID
	}
	return a.ThumbBlobID
}
