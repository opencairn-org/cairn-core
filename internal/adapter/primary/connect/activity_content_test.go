package connect

import (
	"context"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

type fakeAttachments struct {
	port.AttachmentRepo
	byActivity map[domain.ActivityID][]domain.Attachment
}

func (f *fakeAttachments) ListForActivity(_ context.Context, id domain.ActivityID) ([]domain.Attachment, error) {
	return f.byActivity[id], nil
}

type fakeStreams struct {
	port.StreamRepo
	samples []domain.StreamSample
}

func (f *fakeStreams) QueryStream(_ context.Context, q domain.StreamQuery) (domain.Stream, error) {
	return domain.Stream{ActivitySourceID: q.ActivitySourceID, Samples: f.samples}, nil
}

func f64(v float64) *float64 { return &v }

// contentFixture: an owner with one activity that has a GPS stream and two photos.
func contentFixture(t *testing.T) (*actFixture, domain.ActivityID) {
	t.Helper()
	f := newActFixture(t)
	sid, aid := seedActivity(f.repo, f.user, "garmin", actNow, 10000)
	act := f.repo.Activities[aid]
	act.PrimaryStreamSourceID = &sid
	act.MergedAt = actNow
	f.repo.Activities[aid] = act
	f.srv.Attachments = &fakeAttachments{byActivity: map[domain.ActivityID][]domain.Attachment{aid: {
		{ID: domain.AttachmentID(uuid.New()), ActivityID: aid, BlobID: "attachments/a/1", ContentType: "image/png", Width: 2000, Height: 1000,
			ThumbBlobID: "attachments/a/1.thumb.jpg", MediumBlobID: "attachments/a/1.medium.jpg", Blurhash: "LEHV6nWB2yk8", Caption: "sunrise", CreatedAt: actNow},
		{ID: domain.AttachmentID(uuid.New()), ActivityID: aid, BlobID: "attachments/a/2", ContentType: "image/jpeg", CreatedAt: actNow}, // no variants yet
	}}}
	f.srv.Visibility = view.NewVisibility(nil, nil, nil)
	f.srv.Exports = view.NewExports(&fakeStreams{samples: []domain.StreamSample{
		{Timestamp: actNow, Latitude: f64(49.0), Longitude: f64(8.0), HeartRateBpm: new(int16)},
		{Timestamp: actNow.Add(time.Second), Latitude: f64(49.001), Longitude: f64(8.001)},
	}})
	f.srv.MapETag = func(a domain.Activity) string { return "v5-" + a.ID.String()[:4] }
	return f, aid
}

func TestListAttachmentsSignsVariants(t *testing.T) {
	f, aid := contentFixture(t)
	res, err := f.srv.ListAttachments(f.ctx, &v1.ListAttachmentsRequest{ActivityId: aid.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetAttachments()) != 2 {
		t.Fatalf("got %d attachments", len(res.GetAttachments()))
	}
	a := res.GetAttachments()[0]
	if a.GetOriginal().GetUrl() != "https://c.test/blob/attachments/a/1" || a.GetMedium().GetUrl() != "https://c.test/blob/attachments/a/1.medium.jpg" ||
		a.GetThumb().GetUrl() != "https://c.test/blob/attachments/a/1.thumb.jpg" || a.GetBlurhash() != "LEHV6nWB2yk8" || a.GetWidth() != 2000 || a.GetCaption() != "sunrise" {
		t.Fatalf("attachment %v", a)
	}
	b := res.GetAttachments()[1]
	if b.GetOriginal() == nil || b.Medium != nil || b.Thumb != nil || b.GetBlurhash() != "" {
		t.Fatalf("variant-less attachment %v", b)
	}
	// A stranger: the activity is private → NotFound.
	other := context.WithValue(t.Context(), ctxKeyUserID, domain.UserID(uuid.New()))
	_, err = f.srv.ListAttachments(other, &v1.ListAttachmentsRequest{ActivityId: aid.String()})
	wantCode(t, err, connectrpc.CodeNotFound)
	_, err = f.srv.ListAttachments(t.Context(), &v1.ListAttachmentsRequest{ActivityId: aid.String()})
	wantCode(t, err, connectrpc.CodeNotFound)
}

func TestGetActivityMapImageAndListCarryMapImage(t *testing.T) {
	f, aid := contentFixture(t)
	res, err := f.srv.GetActivityMapImage(f.ctx, &v1.GetActivityMapImageRequest{ActivityId: aid.String()})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetImage().GetUrl() != "https://c.test/map/"+aid.String()+"/full" || res.GetEtag() != "v5-"+aid.String()[:4] {
		t.Fatalf("response %v", res)
	}
	got, err := f.srv.GetActivity(f.ctx, &v1.GetActivityRequest{Id: aid.String()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetActivity().GetMapImage().GetUrl() != res.GetImage().GetUrl() || got.GetActivity().GetMapImageEtag() != res.GetEtag() {
		t.Fatalf("GetActivity map image %v", got.GetActivity().GetMapImage())
	}
	// No track → NotFound, and no map_image on the wire.
	act := f.repo.Activities[aid]
	act.PrimaryStreamSourceID = nil
	f.repo.Activities[aid] = act
	_, err = f.srv.GetActivityMapImage(f.ctx, &v1.GetActivityMapImageRequest{ActivityId: aid.String()})
	wantCode(t, err, connectrpc.CodeNotFound)
	got, _ = f.srv.GetActivity(f.ctx, &v1.GetActivityRequest{Id: aid.String()})
	if got.GetActivity().MapImage != nil {
		t.Fatal("map image must be unset without a track")
	}
}

func TestExportOptionsAndExportActivity(t *testing.T) {
	f, aid := contentFixture(t)
	opts, err := f.srv.GetExportOptions(f.ctx, &v1.GetExportOptionsRequest{ActivityId: aid.String()})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.GetAvailable()["gps"] || !opts.GetAvailable()["heart_rate"] || opts.GetAvailable()["power"] {
		t.Fatalf("available %v", opts.GetAvailable())
	}
	res, err := f.srv.ExportActivity(f.ctx, &v1.ExportActivityRequest{ActivityId: aid.String(), Format: v1.ExportFormat_EXPORT_FORMAT_GPX, Exclude: []string{"heart_rate"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetFile().GetUrl() != "https://c.test/export/"+aid.String()+".gpx" || res.GetContentType() != "application/gpx+xml" || res.GetFilename() == "" {
		t.Fatalf("response %v", res)
	}
	_, err = f.srv.ExportActivity(f.ctx, &v1.ExportActivityRequest{ActivityId: aid.String(), Format: v1.ExportFormat_EXPORT_FORMAT_UNSPECIFIED})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	_, err = f.srv.ExportActivity(f.ctx, &v1.ExportActivityRequest{ActivityId: aid.String(), Format: v1.ExportFormat_EXPORT_FORMAT_FIT, Exclude: []string{"nope"}})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	// Non-owner: exports are owner-only even for a visible activity.
	other := context.WithValue(t.Context(), ctxKeyUserID, domain.UserID(uuid.New()))
	_, err = f.srv.ExportActivity(other, &v1.ExportActivityRequest{ActivityId: aid.String(), Format: v1.ExportFormat_EXPORT_FORMAT_GPX})
	wantCode(t, err, connectrpc.CodeNotFound)
}
