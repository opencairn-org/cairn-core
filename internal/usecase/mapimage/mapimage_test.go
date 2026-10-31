package mapimage

import (
	"context"
	"errors"
	"image"
	"image/color"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/staticmap"
)

type fakeStreams struct {
	port.StreamRepo
	samples []domain.StreamSample
	calls   int
}

func (f *fakeStreams) QueryStream(context.Context, domain.StreamQuery) (domain.Stream, error) {
	f.calls++
	return domain.Stream{Samples: f.samples}, nil
}

type fakeBlobs struct {
	port.BlobStore
	objects map[string][]byte
}

func (b *fakeBlobs) Get(_ context.Context, key string) ([]byte, string, error) {
	d, ok := b.objects[key]
	if !ok {
		return nil, "", errors.New("missing")
	}
	return d, "image/png", nil
}
func (b *fakeBlobs) Put(_ context.Context, key string, data []byte, _ string) error {
	b.objects[key] = data
	return nil
}
func (b *fakeBlobs) Delete(_ context.Context, key string) error {
	delete(b.objects, key)
	return nil
}

// allZoned puts every point inside a privacy zone for non-owners.
type allZoned struct{}

func (allZoned) OwnerZones(_ context.Context, _ domain.UserID, isOwner bool) []domain.PrivacyZone {
	if isOwner {
		return nil
	}
	return []domain.PrivacyZone{{Lat: 47, Lng: 8, RadiusM: 100000}}
}

func grayTile(context.Context, int, int, int) (image.Image, error) {
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(0, 0, color.Black)
	return img, nil
}

func track() []domain.StreamSample {
	var out []domain.StreamSample
	for i := range 20 {
		lat, lng := 47+float64(i)*0.001, 8+float64(i)*0.001
		out = append(out, domain.StreamSample{Latitude: &lat, Longitude: &lng})
	}
	return out
}

func activity() domain.Activity {
	sid := domain.SourceID(uuid.New())
	return domain.Activity{ID: domain.ActivityID(uuid.New()), UserID: domain.UserID(uuid.New()), PrimaryStreamSourceID: &sid, MergedAt: time.Unix(1000, 0)}
}

func TestLoadRendersOnceAndCaches(t *testing.T) {
	ctx := context.Background()
	streams := &fakeStreams{samples: track()}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	m := New(streams, blobs, allZoned{}, staticmap.TileFetcher(grayTile), nil)
	act := activity()

	data, ok := m.Load(ctx, act, false)
	if !ok || len(data) == 0 || blobs.objects[CacheKey(act.ID, false)] == nil {
		t.Fatalf("render: ok=%v cached=%v", ok, blobs.objects[CacheKey(act.ID, false)] != nil)
	}
	if _, ok := m.Load(ctx, act, false); !ok || streams.calls != 1 {
		t.Fatalf("second load re-rendered: calls=%d", streams.calls)
	}
	// Every point inside a zone: no trimmed variant.
	if _, ok := m.Load(ctx, act, true); ok {
		t.Fatal("fully zoned track rendered")
	}
	if !m.Regenerate(ctx, act) || streams.calls != 3 {
		t.Fatalf("regenerate: calls=%d", streams.calls)
	}
	noTrack := activity()
	noTrack.PrimaryStreamSourceID = nil
	if _, ok := m.Load(ctx, noTrack, false); ok || ETag(noTrack) != "" {
		t.Fatal("activity without a track rendered")
	}
}

func TestCacheKeyAndETag(t *testing.T) {
	act := activity()
	if got := CacheKey(act.ID, true); got != "mapimg/"+StyleVersion+"/"+act.ID.String()+"/trimmed.png" {
		t.Fatalf("key %q", got)
	}
	if got := ETag(act); got != StyleVersion+"-"+act.PrimaryStreamSourceID.String()[:8]+"-rs" {
		t.Fatalf("etag %q", got)
	}
}

func TestDownsampleKeepsEnds(t *testing.T) {
	pts := make([]staticmap.LatLng, 1000)
	for i := range pts {
		pts[i] = staticmap.LatLng{Lat: float64(i)}
	}
	out := downsample(pts, 600)
	if len(out) != 600 || out[0].Lat != 0 || out[599].Lat != 999 {
		t.Fatalf("len=%d first=%v last=%v", len(out), out[0], out[len(out)-1])
	}
	if len(downsample(pts[:10], 600)) != 10 {
		t.Fatal("short track changed")
	}
}
