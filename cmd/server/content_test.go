package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

func TestContentCodecRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	c := newContentCodec("session-secret", func() time.Time { return now })
	cl := contentClaims{Kind: contentKindBlob, Key: "attachments/u/a/x", ContentType: "image/jpeg", Expires: now.Add(time.Minute).Unix()}
	tok, err := c.encode(cl)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.decode(tok)
	if err != nil || got != cl {
		t.Fatalf("decode: %+v %v", got, err)
	}
	// Tampered payload, tampered signature, other secret, expired: all refused.
	payload, sig, _ := strings.Cut(tok, ".")
	for name, bad := range map[string]string{
		"payload":   payload + "x." + sig,
		"signature": payload + "." + sig[:len(sig)-2] + "zz",
		"shape":     payload,
		"empty":     "",
	} {
		if _, err := c.decode(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := newContentCodec("other", func() time.Time { return now }).decode(tok); err == nil {
		t.Error("other secret accepted")
	}
	late := newContentCodec("session-secret", func() time.Time { return now.Add(2 * time.Minute) })
	if _, err := late.decode(tok); err == nil {
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

type routeBlobs struct{ port.BlobStore }

func (routeBlobs) PresignPublicDownload(context.Context, string, port.PresignDownloadOpts) (port.PresignedURL, error) {
	return port.PresignedURL{}, port.ErrNoPublicEndpoint
}
func (routeBlobs) PresignPublicUpload(context.Context, string, port.PresignUploadOpts) (port.PresignedURL, error) {
	return port.PresignedURL{}, port.ErrNoPublicEndpoint
}

func newTestSigner(blobs port.BlobStore) *contentSigner {
	cfg := &config.Config{}
	cfg.Auth.SessionSecret = "s"
	cfg.HTTP.PublicBaseURL = "https://cairn.test/"
	cfg.Storage.PresignTTL = 10 * time.Minute
	return newContentSigner(&App{BlobStore: blobs}, cfg)
}

func TestContentSignerPicksPresignOrRoute(t *testing.T) {
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
	cl, err := rt.codec.decode(strings.TrimPrefix(u.URL, "https://cairn.test/content/"))
	if err != nil || cl.Kind != contentKindBlob || cl.Key != "attachments/x" || cl.Filename != "photo.jpg" {
		t.Fatalf("claims %+v %v", cl, err)
	}
	if time.Until(u.ExpiresAt) > 10*time.Minute || time.Until(u.ExpiresAt) < 9*time.Minute {
		t.Fatalf("ttl %v", time.Until(u.ExpiresAt))
	}
	up, err := rt.Upload(ctx, "uploads/u/f.fit", "application/vnd.ant.fit", 123)
	if err != nil || up.Method != "PUT" || !strings.HasPrefix(up.URL, "https://cairn.test/content/") || up.RequiredHeaders["Content-Type"] != "application/vnd.ant.fit" {
		t.Fatalf("route upload: %+v %v", up, err)
	}
	cl, _ = rt.codec.decode(strings.TrimPrefix(up.URL, "https://cairn.test/content/"))
	if cl.Kind != contentKindUpload || cl.MaxBytes != 123 {
		t.Fatalf("upload claims %+v", cl)
	}
	ex, err := rt.Export(ctx, id, "gpx", []string{"heart_rate", "power"})
	if err != nil {
		t.Fatal(err)
	}
	cl, _ = rt.codec.decode(strings.TrimPrefix(ex.URL, "https://cairn.test/content/"))
	if cl.Kind != contentKindExport || cl.Format != "gpx" || cl.Exclude != "heart_rate,power" || cl.ActivityID != id.String() {
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
}

func TestNormalizePathCollapsesContentTokens(t *testing.T) {
	if got := normalizePath("/content/abc.def"); got != "/content/:token" {
		t.Fatalf("got %q", got)
	}
	if got := normalizePath("/api/activities/" + uuid.NewString() + "/map.png"); got != "/api/activities/:id/map.png" {
		t.Fatalf("got %q", got)
	}
}
