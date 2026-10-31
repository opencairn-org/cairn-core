package attachment

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type memBlobs struct {
	port.BlobStore
	objects map[string][]byte
}

func (m *memBlobs) Put(_ context.Context, key string, data []byte, _ string) error {
	m.objects[key] = data
	return nil
}
func (m *memBlobs) Get(_ context.Context, key string) ([]byte, string, error) {
	b, ok := m.objects[key]
	if !ok {
		return nil, "", errors.New("missing")
	}
	return b, "image/png", nil
}

type memAttachments struct {
	port.AttachmentRepo
	rows map[domain.AttachmentID]domain.Attachment
}

func (m *memAttachments) ListForActivity(_ context.Context, id domain.ActivityID) ([]domain.Attachment, error) {
	var out []domain.Attachment
	for _, a := range m.rows {
		if a.ActivityID == id {
			out = append(out, a)
		}
	}
	return out, nil
}
func (m *memAttachments) SetVariants(_ context.Context, id domain.AttachmentID, thumb, medium, blurhash string, w, h int) error {
	a := m.rows[id]
	a.ThumbBlobID, a.MediumBlobID, a.Blurhash, a.Width, a.Height = thumb, medium, blurhash, w, h
	m.rows[id] = a
	return nil
}
func (m *memAttachments) ListWithoutVariants(_ context.Context, limit int) ([]domain.Attachment, error) {
	var out []domain.Attachment
	for _, a := range m.rows {
		if !a.HasVariants() && len(out) < limit {
			out = append(out, a)
		}
	}
	return out, nil
}

func samplePNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestVariantsDeriveAndEnsure(t *testing.T) {
	blobs := &memBlobs{objects: map[string][]byte{}}
	repo := &memAttachments{rows: map[domain.AttachmentID]domain.Attachment{}}
	uc := NewVariants(repo, blobs, nil)

	a := domain.Attachment{ID: domain.AttachmentID(uuid.New()), ActivityID: domain.ActivityID(uuid.New()), BlobID: "attachments/u/a/1"}
	got, err := uc.Derive(context.Background(), a, samplePNG(1600, 800))
	if err != nil {
		t.Fatal(err)
	}
	if got.ThumbBlobID != "attachments/u/a/1.thumb.jpg" || got.MediumBlobID != "attachments/u/a/1.medium.jpg" || got.Blurhash == "" || got.Width != 1600 {
		t.Fatalf("derived %+v", got)
	}
	if len(blobs.objects) != 2 {
		t.Fatalf("stored %d variant objects", len(blobs.objects))
	}
	if _, err := uc.Derive(context.Background(), a, []byte("junk")); !errors.Is(err, ErrNotImage) {
		t.Fatalf("junk: %v", err)
	}

	// Ensure derives for a stored original and records the keys.
	b := domain.Attachment{ID: domain.AttachmentID(uuid.New()), ActivityID: a.ActivityID, BlobID: "attachments/u/a/2"}
	blobs.objects[b.BlobID] = samplePNG(200, 100)
	repo.rows[b.ID] = b
	if err := uc.Ensure(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if r := repo.rows[b.ID]; !r.HasVariants() || r.MediumBlobID == "" || r.Width != 200 {
		t.Fatalf("ensured %+v", r)
	}
	// Already derived: no new objects.
	n := len(blobs.objects)
	if err := uc.Ensure(context.Background(), repo.rows[b.ID]); err != nil || len(blobs.objects) != n {
		t.Fatalf("second ensure: %v, objects %d", err, len(blobs.objects))
	}
}

func TestVariantsBackfillCountsAndStops(t *testing.T) {
	blobs := &memBlobs{objects: map[string][]byte{}}
	repo := &memAttachments{rows: map[domain.AttachmentID]domain.Attachment{}}
	act := domain.ActivityID(uuid.New())
	for i := range 3 {
		id := domain.AttachmentID(uuid.New())
		key := "attachments/x/" + id.String()
		repo.rows[id] = domain.Attachment{ID: id, ActivityID: act, BlobID: key}
		if i < 2 {
			blobs.objects[key] = samplePNG(50, 50)
		} // the third original is missing → fails, must not loop forever
	}
	res, err := NewVariants(repo, blobs, nil).Backfill(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 2 || res.Failed != 1 {
		t.Fatalf("result %+v", res)
	}
	if _, err := NewVariants(repo, nil, nil).Backfill(context.Background(), 2); err == nil {
		t.Fatal("no storage must be an error")
	}
}
