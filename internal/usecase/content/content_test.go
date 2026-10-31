package content

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

func TestCodecRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	c := NewCodec("session-secret", func() time.Time { return now })
	cl := Claims{Kind: KindBlob, Key: "attachments/u/a/x", ContentType: "image/jpeg", Expires: now.Add(time.Minute).Unix()}
	tok, err := c.Encode(cl)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Decode(tok)
	if err != nil || got != cl {
		t.Fatalf("decode: %+v %v", got, err)
	}
	if c.Remaining(cl) != 60 {
		t.Fatalf("remaining %d", c.Remaining(cl))
	}
	// Tampered payload, tampered signature, other secret, expired: all refused.
	payload, sig, _ := strings.Cut(tok, ".")
	for name, bad := range map[string]string{
		"payload":   payload + "x." + sig,
		"signature": payload + "." + sig[:len(sig)-2] + "zz",
		"shape":     payload,
		"empty":     "",
	} {
		if _, err := c.Decode(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewCodec("other", func() time.Time { return now }).Decode(tok); err == nil {
		t.Error("other secret accepted")
	}
	late := NewCodec("session-secret", func() time.Time { return now.Add(2 * time.Minute) })
	if _, err := late.Decode(tok); err == nil {
		t.Error("expired token accepted")
	}
}

// presignBlobs offers a public endpoint; routeBlobs does not.
type presignBlobs struct{ port.BlobStore }

func (presignBlobs) PresignPublicDownload(_ context.Context, key string, opts port.PresignDownloadOpts) (port.PresignedURL, error) {
	return port.PresignedURL{URL: "https://s3.public/" + key, ExpiresAt: time.Now().Add(opts.Expiry)}, nil
}
func (presignBlobs) PresignPublicUpload(_ context.Context, key string, _ port.PresignUploadOpts) (port.PresignedURL, error) {
	return port.PresignedURL{URL: "https://s3.public/" + key, Method: "PUT"}, nil
}

type routeBlobs struct {
	port.BlobStore
	objects map[string][]byte
	types   map[string]string
}

func (routeBlobs) PresignPublicDownload(context.Context, string, port.PresignDownloadOpts) (port.PresignedURL, error) {
	return port.PresignedURL{}, port.ErrNoPublicEndpoint
}
func (routeBlobs) PresignPublicUpload(context.Context, string, port.PresignUploadOpts) (port.PresignedURL, error) {
	return port.PresignedURL{}, port.ErrNoPublicEndpoint
}
func (b routeBlobs) Get(_ context.Context, key string) ([]byte, string, error) {
	d, ok := b.objects[key]
	if !ok {
		return nil, "", errors.New("missing")
	}
	return d, b.types[key], nil
}
func (b routeBlobs) Put(_ context.Context, key string, data []byte, ct string) error {
	b.objects[key], b.types[key] = data, ct
	return nil
}

func newTestSigner(blobs port.BlobStore) *Signer {
	return NewSigner(NewCodec("s", nil), "https://cairn.test/", 10*time.Minute, blobs)
}

func TestSignerPicksPresignOrRoute(t *testing.T) {
	ctx := context.Background()
	id := domain.ActivityID(uuid.New())

	pre := newTestSigner(presignBlobs{})
	if u, err := pre.Blob(ctx, "k", "image/png", ""); err != nil || u.URL != "https://s3.public/k" {
		t.Fatalf("presign blob: %v %v", u, err)
	}
	if u, err := pre.Upload(ctx, "k", "image/png", 10); err != nil || u.URL != "https://s3.public/k" || u.Method != "PUT" {
		t.Fatalf("presign upload: %v %v", u, err)
	}
	// Rendered content always goes through the server, even with a public store.
	if u, err := pre.MapImage(ctx, id, true); err != nil || !strings.HasPrefix(u.URL, "https://cairn.test/content/") {
		t.Fatalf("map: %v %v", u, err)
	}

	rt := newTestSigner(routeBlobs{})
	u, err := rt.Blob(ctx, "attachments/x", "image/jpeg", "photo.jpg")
	if err != nil || !strings.HasPrefix(u.URL, "https://cairn.test/content/") {
		t.Fatalf("route blob: %v %v", u, err)
	}
	cl, err := rt.Codec.Decode(strings.TrimPrefix(u.URL, "https://cairn.test/content/"))
	if err != nil || cl.Kind != KindBlob || cl.Key != "attachments/x" || cl.Filename != "photo.jpg" {
		t.Fatalf("claims %+v %v", cl, err)
	}
	if time.Until(u.ExpiresAt) > 10*time.Minute || time.Until(u.ExpiresAt) < 9*time.Minute {
		t.Fatalf("ttl %v", time.Until(u.ExpiresAt))
	}
	up, err := rt.Upload(ctx, "uploads/u/f.fit", "application/vnd.ant.fit", 123)
	if err != nil || up.Method != "PUT" || !strings.HasPrefix(up.URL, "https://cairn.test/content/") || up.RequiredHeaders["Content-Type"] != "application/vnd.ant.fit" {
		t.Fatalf("route upload: %+v %v", up, err)
	}
	cl, _ = rt.Codec.Decode(strings.TrimPrefix(up.URL, "https://cairn.test/content/"))
	if cl.Kind != KindUpload || cl.MaxBytes != 123 {
		t.Fatalf("upload claims %+v", cl)
	}
	ex, err := rt.Export(ctx, id, "gpx", []string{"heart_rate", "power"})
	if err != nil {
		t.Fatal(err)
	}
	cl, _ = rt.Codec.Decode(strings.TrimPrefix(ex.URL, "https://cairn.test/content/"))
	if cl.Kind != KindExport || cl.Format != "gpx" || cl.Exclude != "heart_rate,power" || cl.ActivityID != id.String() {
		t.Fatalf("export claims %+v", cl)
	}

	// No storage at all: blobs still route, uploads are refused.
	none := newTestSigner(nil)
	if _, err := none.Blob(ctx, "k", "", ""); err != nil {
		t.Fatalf("route without storage: %v", err)
	}
	if _, err := none.Upload(ctx, "k", "", 1); err == nil {
		t.Fatal("upload without storage must fail")
	}
	if none.HasStorage() {
		t.Fatal("no storage reported as storage")
	}
}

func TestSignerReadWrite(t *testing.T) {
	ctx := context.Background()
	b := routeBlobs{objects: map[string][]byte{}, types: map[string]string{}}
	s := newTestSigner(b)
	// The claimed content type wins over the request's.
	if err := s.Write(ctx, Claims{Kind: KindUpload, Key: "u", ContentType: "application/gpx+xml"}, []byte("x"), "text/plain"); err != nil || b.types["u"] != "application/gpx+xml" {
		t.Fatalf("write: %v %q", err, b.types["u"])
	}
	if err := s.Write(ctx, Claims{Kind: KindUpload, Key: "v"}, []byte("y"), "text/plain"); err != nil || b.types["v"] != "text/plain" {
		t.Fatalf("write fallback: %v %q", err, b.types["v"])
	}
	if _, ct, err := s.Read(ctx, Claims{Key: "u", ContentType: "image/png"}); err != nil || ct != "image/png" {
		t.Fatalf("read override: %q %v", ct, err)
	}
	b.types["w"], b.objects["w"] = "", []byte("z")
	if _, ct, err := s.Read(ctx, Claims{Key: "w"}); err != nil || ct != "application/octet-stream" {
		t.Fatalf("read default type: %q %v", ct, err)
	}
	if _, _, err := s.Read(ctx, Claims{Key: "gone"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, _, err := newTestSigner(nil).Read(ctx, Claims{Key: "u"}); !errors.Is(err, ErrNoStorage) {
		t.Fatalf("no storage: %v", err)
	}
}
