package connect

import (
	"context"
	"fmt"
	"strings"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// ---------------------------------------------------------------------------
// Photos, course image and exports: binary content is never an RPC payload,
// every RPC here returns signed ContentUrls (see port.ContentSigner).
// ---------------------------------------------------------------------------

func contentURLToProto(u port.ContentURL) *v1.ContentUrl {
	if u.URL == "" {
		return nil
	}
	return &v1.ContentUrl{Url: u.URL, ExpiresAt: timestamppb.New(u.ExpiresAt)}
}

// withMapImage adds the signed course-image URL to a wire activity when the
// activity has a renderable track. trimmed selects the privacy-zone variant.
func (s *ActivityServer) withMapImage(ctx context.Context, out *v1.Activity, a domain.Activity, trimmed bool) *v1.Activity {
	if s.Content == nil || a.PrimaryStreamSourceID == nil || s.MapETag == nil {
		return out
	}
	if u, err := s.Content.MapImage(ctx, a.ID, trimmed); err == nil {
		out.MapImage = contentURLToProto(u)
		out.MapImageEtag = s.MapETag(a)
	}
	return out
}

// viewerFor resolves the calling viewer (nil when anonymous) for a
// visibility-gated read.
func viewerFor(ctx context.Context) *domain.UserID {
	if id, err := userIDFromHandlerCtx(ctx); err == nil {
		return &id
	}
	return nil
}

// visibleActivity loads an activity and resolves how the caller may see it:
// owners fully, others through the visibility model. NotFound hides private,
// hidden, deleted and foreign-blocked activities alike.
func (s *ActivityServer) visibleActivity(ctx context.Context, rawID string) (domain.Activity, view.Viewer, error) {
	id, err := parseActivityID(rawID)
	if err != nil {
		return domain.Activity{}, view.Viewer{}, err
	}
	act, err := s.Activities.GetActivity(ctx, id)
	if err != nil {
		return domain.Activity{}, view.Viewer{}, notFoundErr(err)
	}
	if s.Visibility == nil {
		return domain.Activity{}, view.Viewer{}, connectrpc.NewError(connectrpc.CodeUnavailable, "visibility is not available on this instance")
	}
	vw, ok := s.Visibility.Resolve(ctx, act, viewerFor(ctx), false)
	if !ok {
		return domain.Activity{}, view.Viewer{}, connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	}
	return act, vw, nil
}

// ListAttachments: the owner, or a viewer the photos category is open to.
func (s *ActivityServer) ListAttachments(ctx context.Context, req *v1.ListAttachmentsRequest) (*v1.ListAttachmentsResponse, error) {
	if s.Attachments == nil || s.Content == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "attachments are not available on this instance")
	}
	act, vw, err := s.visibleActivity(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	if !vw.IsOwner && !vw.Cats.Has(domain.CategoryPhotos) {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	}
	atts, err := s.Attachments.ListForActivity(ctx, act.ID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("list attachments: %w", err))
	}
	out := &v1.ListAttachmentsResponse{Attachments: make([]*v1.Attachment, 0, len(atts))}
	for _, a := range atts {
		out.Attachments = append(out.Attachments, s.attachmentToProto(ctx, a))
	}
	return out, nil
}

func (s *ActivityServer) attachmentToProto(ctx context.Context, a domain.Attachment) *v1.Attachment {
	out := &v1.Attachment{
		Id:          a.ID.String(),
		ActivityId:  a.ActivityID.String(),
		Width:       connectClampInt32(a.Width),
		Height:      connectClampInt32(a.Height),
		Blurhash:    a.Blurhash,
		Caption:     a.Caption,
		ContentType: a.ContentType,
		CreatedAt:   timestamppb.New(a.CreatedAt),
	}
	if u, err := s.Content.Blob(ctx, a.BlobID, a.ContentType, ""); err == nil {
		out.Original = contentURLToProto(u)
	}
	if a.MediumBlobID != "" {
		if u, err := s.Content.Blob(ctx, a.MediumBlobID, "image/jpeg", ""); err == nil {
			out.Medium = contentURLToProto(u)
		}
	}
	if a.ThumbBlobID != "" {
		if u, err := s.Content.Blob(ctx, a.ThumbBlobID, "image/jpeg", ""); err == nil {
			out.Thumb = contentURLToProto(u)
		}
	}
	return out
}

// GetActivityMapImage: owner gets the full track, a viewer the map category
// is open to gets the privacy-zone trimmed one.
func (s *ActivityServer) GetActivityMapImage(ctx context.Context, req *v1.GetActivityMapImageRequest) (*v1.GetActivityMapImageResponse, error) {
	if s.Content == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "course images are not available on this instance")
	}
	act, vw, err := s.visibleActivity(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	if !vw.IsOwner && !vw.Cats.Has(domain.CategoryMap) {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	}
	if act.PrimaryStreamSourceID == nil {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound, "this activity has no GPS track")
	}
	u, err := s.Content.MapImage(ctx, act.ID, !vw.IsOwner)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("sign map image: %w", err))
	}
	out := &v1.GetActivityMapImageResponse{Image: contentURLToProto(u)}
	if s.MapETag != nil {
		out.Etag = s.MapETag(act)
	}
	return out, nil
}

// GetExportOptions: owner only.
func (s *ActivityServer) GetExportOptions(ctx context.Context, req *v1.GetExportOptionsRequest) (*v1.GetExportOptionsResponse, error) {
	if s.Exports == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "export is not available on this instance")
	}
	act, err := s.activityUpdateLoad(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	available, err := s.Exports.Available(ctx, act)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.GetExportOptionsResponse{Available: available}, nil
}

// ExportActivity: owner only; validates the request so the signed URL is
// known to render, then hands out the URL.
func (s *ActivityServer) ExportActivity(ctx context.Context, req *v1.ExportActivityRequest) (*v1.ExportActivityResponse, error) {
	if s.Exports == nil || s.Content == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "export is not available on this instance")
	}
	act, err := s.activityUpdateLoad(ctx, req.GetActivityId())
	if err != nil {
		return nil, err
	}
	format := exportFormatFromProto(req.GetFormat())
	if format == "" {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "format must be gpx, tcx or fit")
	}
	exclude := strings.Join(req.GetExclude(), ",")
	// Render once now: a bad exclude token or a stream-less activity fails
	// here with a typed error instead of a 404 behind the URL.
	file, err := s.Exports.Render(ctx, act, format, exclude)
	if err != nil {
		return nil, actionErr(err)
	}
	u, err := s.Content.Export(ctx, act.ID, format, req.GetExclude())
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("sign export: %w", err))
	}
	return &v1.ExportActivityResponse{File: contentURLToProto(u), Filename: file.Filename, ContentType: file.ContentType}, nil
}

func exportFormatFromProto(f v1.ExportFormat) string {
	switch f {
	case v1.ExportFormat_EXPORT_FORMAT_GPX:
		return "gpx"
	case v1.ExportFormat_EXPORT_FORMAT_TCX:
		return "tcx"
	case v1.ExportFormat_EXPORT_FORMAT_FIT:
		return "fit"
	case v1.ExportFormat_EXPORT_FORMAT_UNSPECIFIED:
	}
	return ""
}
