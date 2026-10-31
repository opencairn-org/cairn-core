package connect

import (
	"context"
	"fmt"
	"strings"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ---------------------------------------------------------------------------
// Two-step file upload. PresignActivityUpload hands out a PUT URL under the
// caller's own prefix; UploadActivity reads the object back and runs the same
// ingest as POST /api/activities/upload. Ingest is synchronous, so the
// response already carries the activity id and job_id stays empty.
// ---------------------------------------------------------------------------

// uploadContentTypes maps the accepted extensions to the archived content type.
var uploadContentTypes = map[string]string{
	"fit": "application/vnd.ant.fit",
	"gpx": "application/gpx+xml",
	"tcx": "application/vnd.garmin.tcx+xml",
}

// uploadKeyPrefix scopes presigned uploads to the caller, so a blob id can be
// checked against the session on UploadActivity.
func uploadKeyPrefix(userID domain.UserID) string {
	return "uploads/" + userID.String() + "/"
}

func (s *ActivityServer) PresignActivityUpload(ctx context.Context, req *v1.PresignActivityUploadRequest) (*v1.PresignActivityUploadResponse, error) {
	if s.Blobs == nil || s.Content == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "object storage is not configured; use the REST upload instead")
	}
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(strings.TrimPrefix(req.GetExtension(), "."))
	ct, ok := uploadContentTypes[ext]
	if !ok {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "extension must be one of fit, gpx, tcx")
	}
	if req.GetContentType() != "" {
		ct = req.GetContentType()
	}
	if hint := req.GetSizeHintBytes(); hint < 0 || hint > port.MaxUploadBytes {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, fmt.Sprintf("size_hint_bytes must be between 0 and %d", port.MaxUploadBytes))
	}
	key := uploadKeyPrefix(userID) + uuid.NewString() + "." + ext
	// A public presigned PUT when the store has a client-reachable endpoint,
	// else the server's own signed PUT route: either way the client just PUTs.
	signed, err := s.Content.Upload(ctx, key, ct, port.MaxUploadBytes)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.PresignActivityUploadResponse{
		BlobId:          key,
		UploadUrl:       signed.URL,
		RequiredHeaders: signed.RequiredHeaders,
		ExpiresAt:       timestamppb.New(signed.ExpiresAt),
	}, nil
}

func (s *ActivityServer) UploadActivity(ctx context.Context, req *v1.UploadActivityRequest) (*v1.UploadActivityResponse, error) {
	if s.Blobs == nil || s.Uploads == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "object storage is not configured; use the REST upload instead")
	}
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	key := req.GetBlobId()
	if !strings.HasPrefix(key, uploadKeyPrefix(userID)) || strings.Contains(key, "..") {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound, "unknown blob_id")
	}
	raw, _, err := s.Blobs.Get(ctx, key)
	if err != nil {
		return nil, connectError(connectrpc.CodeNotFound, fmt.Errorf("blob not uploaded yet: %w", err))
	}
	filename := req.GetFilename()
	if filename == "" {
		filename = key[strings.LastIndex(key, "/")+1:]
	}
	out, err := s.Uploads.IngestUpload(ctx, port.UploadInput{UserID: userID, Filename: filename, Raw: raw, RawBlobKey: key})
	if err != nil {
		return nil, actionErr(err)
	}
	if err := s.applyUploadHints(ctx, out.ActivityID, req); err != nil {
		return nil, err
	}
	return &v1.UploadActivityResponse{ActivityId: out.ActivityID.String()}, nil
}

// applyUploadHints stores the optional tags and type/discipline hints the way
// UpdateActivity would: tags on the row (user-owned, survive re-merge), the
// classification as an overlay, followed by one recompute.
func (s *ActivityServer) applyUploadHints(ctx context.Context, id domain.ActivityID, req *v1.UploadActivityRequest) error {
	hasClass := req.TypeHint != nil || req.DisciplineHint != nil
	if len(req.GetTags()) == 0 && !hasClass {
		return nil
	}
	if err := s.applyUploadTags(ctx, id, req.GetTags()); err != nil {
		return err
	}
	if hasClass {
		if err := s.applyUploadClass(ctx, id, req); err != nil {
			return err
		}
	}
	if err := s.remerge(ctx, id); err != nil {
		return connectError(connectrpc.CodeInternal, fmt.Errorf("recompute activity: %w", err))
	}
	return nil
}

func (s *ActivityServer) applyUploadTags(ctx context.Context, id domain.ActivityID, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	act, err := s.Activities.GetActivity(ctx, id)
	if err != nil {
		return connectError(connectrpc.CodeInternal, fmt.Errorf("reload activity: %w", err))
	}
	act.Tags = mergeTags(act.Tags, tags)
	if err := s.Activities.SaveActivity(ctx, act); err != nil {
		return connectError(connectrpc.CodeInternal, fmt.Errorf("save tags: %w", err))
	}
	return nil
}

func (s *ActivityServer) applyUploadClass(ctx context.Context, id domain.ActivityID, req *v1.UploadActivityRequest) error {
	if s.ClassOverrides == nil {
		return connectrpc.NewError(connectrpc.CodeInvalidArgument, "type_hint/discipline_hint are not editable on this instance")
	}
	co, err := s.ClassOverrides.Get(ctx, id)
	if err != nil {
		return connectError(connectrpc.CodeInternal, err)
	}
	if t, ok := activityTypeFromProto(req.GetTypeHint()); ok {
		co.Type = &t
	}
	if d, ok := disciplineFromProto(req.GetDisciplineHint()); ok {
		co.Discipline = &d
	}
	co.ActivityID = id
	if err := s.ClassOverrides.Set(ctx, co); err != nil {
		return connectError(connectrpc.CodeInternal, fmt.Errorf("save classification override: %w", err))
	}
	return nil
}

// mergeTags appends the new tags not already present, keeping order.
func mergeTags(have, add []string) []string {
	seen := make(map[string]struct{}, len(have))
	out := append([]string(nil), have...)
	for _, t := range have {
		seen[t] = struct{}{}
	}
	for _, t := range add {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
