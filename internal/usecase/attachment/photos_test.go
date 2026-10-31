package attachment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type photoRepo struct {
	port.AttachmentRepo
	rows map[domain.AttachmentID]domain.Attachment
}

func (r *photoRepo) Get(_ context.Context, id domain.AttachmentID) (domain.Attachment, error) {
	a, ok := r.rows[id]
	if !ok {
		return domain.Attachment{}, domain.ErrNotFound
	}
	return a, nil
}
func (r *photoRepo) Add(_ context.Context, a domain.Attachment) error {
	a.ID = domain.AttachmentID(uuid.New())
	r.rows[a.ID] = a
	return nil
}
func (r *photoRepo) Delete(_ context.Context, id domain.AttachmentID) (string, error) {
	a := r.rows[id]
	delete(r.rows, id)
	return a.BlobID, nil
}

type deletingBlobs struct{ *memBlobs }

func (b deletingBlobs) Delete(_ context.Context, key string) error {
	delete(b.objects, key)
	return nil
}

type stubSigner struct{ port.ContentSigner }

func (stubSigner) Blob(_ context.Context, key, _, _ string) (port.ContentURL, error) {
	return port.ContentURL{URL: "signed:" + key, ExpiresAt: time.Unix(0, 0)}, nil
}

func TestPhotosNonOwnersNeverGetTheOriginal(t *testing.T) {
	ctx := context.Background()
	blobs := &memBlobs{objects: map[string][]byte{"orig": []byte("exif"), "orig.medium.jpg": []byte("m")}}
	p := NewPhotos(&photoRepo{rows: map[domain.AttachmentID]domain.Attachment{}}, blobs, nil, stubSigner{}, nil)
	a := domain.Attachment{BlobID: "orig", ContentType: "image/heic", MediumBlobID: "orig.medium.jpg"}

	if data, ct, err := p.Bytes(ctx, a, true); err != nil || string(data) != "exif" || ct != "image/heic" {
		t.Fatalf("owner: %q %q %v", data, ct, err)
	}
	if data, ct, err := p.Bytes(ctx, a, false); err != nil || string(data) != "m" || ct != "image/jpeg" {
		t.Fatalf("viewer: %q %q %v", data, ct, err)
	}
	if _, _, err := p.Bytes(ctx, domain.Attachment{BlobID: "orig"}, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("viewer without variant: %v", err)
	}
	if _, _, err := p.Bytes(ctx, domain.Attachment{BlobID: "gone"}, true); !errors.Is(err, ErrBlobMissing) {
		t.Fatalf("missing blob: %v", err)
	}
	if urls := p.URLs(ctx, a, false); urls["original"].URL != "" || urls["medium"].URL != "signed:orig.medium.jpg" {
		t.Fatalf("viewer urls %+v", urls)
	}
	if urls := p.URLs(ctx, a, true); urls["original"].URL != "signed:orig" {
		t.Fatalf("owner urls %+v", urls)
	}
	if NewPhotos(nil, nil, nil, nil, nil).URLs(ctx, a, true) != nil {
		t.Fatal("urls without a signer")
	}
}

func TestPhotosUploadAndRemove(t *testing.T) {
	ctx := context.Background()
	blobs := deletingBlobs{&memBlobs{objects: map[string][]byte{}}}
	repo := &photoRepo{rows: map[domain.AttachmentID]domain.Attachment{}}
	variants := NewVariants(repo, blobs, nil)
	p := NewPhotos(repo, blobs, variants, nil, nil)
	owner, act := domain.UserID(uuid.New()), domain.ActivityID(uuid.New())

	if err := p.Upload(ctx, owner, act, samplePNG(64, 48), "image/png", "  hi  "); err != nil {
		t.Fatal(err)
	}
	var id domain.AttachmentID
	for k, a := range repo.rows {
		id = k
		if a.Caption != "hi" || a.ThumbBlobID == "" || !strings.HasPrefix(a.BlobID, "attachments/"+owner.String()+"/"+act.String()+"/") {
			t.Fatalf("stored %+v", a)
		}
	}
	if len(blobs.objects) < 2 {
		t.Fatalf("blobs %v", len(blobs.objects))
	}
	if err := p.Remove(ctx, domain.ActivityID(uuid.New()), id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other activity: %v", err)
	}
	if err := p.Remove(ctx, act, id); err != nil || len(repo.rows) != 0 || len(blobs.objects) != 0 {
		t.Fatalf("remove: %v rows=%d blobs=%d", err, len(repo.rows), len(blobs.objects))
	}
	if err := NewPhotos(repo, nil, nil, nil, nil).Upload(ctx, owner, act, nil, "", ""); !errors.Is(err, ErrNoStorage) {
		t.Fatalf("no storage: %v", err)
	}
}
