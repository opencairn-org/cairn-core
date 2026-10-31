// Package connect contains the primary (driving) Connect-RPC adapters
// that wrap the application's use-cases and repositories for cairn.v1.*
// consumers — the SvelteKit frontend, future mobile clients, third-party
// integrations.
//
// The Connect adapter never owns business logic. Each method:
//
//  1. Authenticates the caller (today: X-Cairn-User-ID header; future:
//     session cookie / JWT).
//  2. Translates the proto request into domain inputs.
//  3. Calls into the existing use-case or repository.
//  4. Translates the domain result back into proto.
//
// The cairnv1connect.UnimplementedActivityServiceHandler base is embedded
// so unimplemented RPCs return CodeUnimplemented automatically — the
// surface grows method-by-method without touching every other RPC.
package connect

import (
	"context"
	"errors"
	"fmt"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/activityexport"
	"github.com/opencairn-org/cairn-core/internal/usecase/besteffort"
	"github.com/opencairn-org/cairn-core/internal/usecase/dashboard"
	gearuc "github.com/opencairn-org/cairn-core/internal/usecase/gear"
	matchuc "github.com/opencairn-org/cairn-core/internal/usecase/match"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// ActivityServer implements cairnv1connect.ActivityServiceHandler. The
// embedded Unimplemented base returns CodeUnimplemented for every RPC
// this struct doesn't override.
type ActivityServer struct {
	cairnv1connect.UnimplementedActivityServiceHandler

	Activities  port.ActivityAggregateStore
	Streams     port.StreamRepo
	BestEfforts port.BestEffortRepo

	// Recompute re-runs the merge engine over an activity's sources after a
	// user edit, so the persisted row stays canonical and preserveUserEdits
	// keeps the edit across future source imports.
	Recompute *activity.RecomputeActivityFromSources

	// Editor applies UpdateActivity and the upload hints (nil → Unavailable).
	Editor *activity.Editor

	// Gear resolves the gear names on the wire.
	Gear *gearuc.Service

	// SourceActions and SourceLinks back DeleteActivity, DetachSource and
	// LinkSourceToActivity; the REST handlers call the same use cases.
	SourceActions *activity.SourceActions
	SourceLinks   *matchuc.SourceLinks

	// Reimporter dispatches ReimportSource; Uploads ingests UploadActivity;
	// Blobs mints PresignActivityUpload URLs and reads the uploaded object.
	// Each nil → the RPC answers Unavailable.
	Reimporter port.SourceReimporter
	Uploads    port.UploadIngester
	Blobs      port.BlobStore

	// Events is the live feed behind StreamActivityEvents. Nil → the stream
	// answers Unavailable.
	Events *ActivityEventHub

	// Customs backs the custom best-effort RPCs; Backfill (optional) is told
	// to recompute the user's activities of a type after a create.
	Customs  *besteffort.Customs
	Backfill func(userID domain.UserID, activityType domain.ActivityType)

	// Previews runs reimport dry-runs; PreviewCommitter ingests a held result.
	Previews         *activity.SourcePreviews
	PreviewCommitter port.SourcePreviewCommitter

	// Content signs photo/map/export URLs; Attachments lists photos;
	// Visibility gates non-owner reads; Exports renders interchange files;
	// MapETag derives Activity.map_image_etag. Nil → the RPC is Unavailable
	// (or the field stays empty).
	Content     port.ContentSigner
	Attachments port.AttachmentRepo
	Visibility  *view.Visibility
	Exports     *activityexport.Exports
	Searches    *dashboard.Searches
	Details     *view.Details
	MapETag     func(domain.Activity) string
}

// Compile-time interface check.
var _ cairnv1connect.ActivityServiceHandler = (*ActivityServer)(nil)

// NewActivityServer wires the server with its dependencies. Callers pass
// the concrete repos coming out of wire.go.
func NewActivityServer(
	activities port.ActivityAggregateStore,
	streams port.StreamRepo,
	bestEfforts port.BestEffortRepo,
	recompute *activity.RecomputeActivityFromSources,
	gear *gearuc.Service,
) *ActivityServer {
	return &ActivityServer{
		Activities:  activities,
		Streams:     streams,
		BestEfforts: bestEfforts,
		Recompute:   recompute,
		Gear:        gear,
	}
}

// gearNames resolves the names of the gear attached to the given activities.
func (s *ActivityServer) gearNames(ctx context.Context, activities ...domain.Activity) map[domain.GearID]string {
	return gearNamesOf(ctx, s.Gear, activities...)
}

// withGear fills the gear fields of a wire activity.
func withGear(out *v1.Activity, a domain.Activity, names map[domain.GearID]string) *v1.Activity {
	out.GearSetBy = gearSetByToProto(a.GearSetBy)
	if a.GearID != nil {
		out.GearName = names[*a.GearID]
	}
	return out
}

func gearSetByToProto(s domain.GearSetBy) v1.GearSetBy {
	if s == domain.GearSetByUser {
		return v1.GearSetBy_GEAR_SET_BY_USER
	}
	return v1.GearSetBy_GEAR_SET_BY_MERGE
}

// ---------------------------------------------------------------------------
// ListActivities
//
// Returns the user's activities whose start_time falls in the requested
// window. Pagination is not yet implemented — the underlying repo method
// returns the full match set, which is acceptable for v1 user-volumes.
// When window is empty/zero we default to "last 90 days".
// ---------------------------------------------------------------------------

const defaultListWindow = 90 * 24 * time.Hour

func (s *ActivityServer) ListActivities(
	ctx context.Context,
	req *v1.ListActivitiesRequest,
) (*v1.ListActivitiesResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}

	// No explicit time range → count-based feed (newest N regardless of age),
	// so older imports don't fall out of an arbitrary default window. An
	// explicit range still uses the time-windowed query.
	tr := req.GetTimeRange()
	var activities []domain.Activity
	if tr == nil || (tr.GetFrom() == nil && tr.GetTo() == nil) {
		limit := int(req.GetPage().GetLimit())
		activities, err = s.Activities.ListRecentActivitiesForUser(ctx, userID, limit)
	} else {
		start, end := windowFromRequest(tr)
		activities, err = s.Activities.ListActivitiesForUser(ctx, userID, start, end)
	}
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal,
			fmt.Errorf("list activities: %w", err))
	}
	names := s.gearNames(ctx, activities...)

	out := &v1.ListActivitiesResponse{
		Activities: make([]*v1.Activity, 0, len(activities)),
	}
	for _, a := range activities {
		var sources []domain.ActivitySource
		if req.GetIncludeSources() {
			srcs, err := s.Activities.ListSourcesForActivity(ctx, a.ID)
			if err != nil {
				return nil, connectError(connectrpc.CodeInternal,
					fmt.Errorf("list sources for %s: %w", a.ID, err))
			}
			sources = srcs
		}
		out.Activities = append(out.Activities, s.withMapImage(ctx, withGear(activityToProto(a, sources), a, names), a))
	}

	// Reverse to start-time-DESC unless the caller asked otherwise. The
	// repo currently returns ASC; the proto default is DESC and the
	// frontend feed expects newest first.
	if req.GetSort() != v1.ActivitySort_ACTIVITY_SORT_START_TIME_ASC {
		reverseActivities(out.Activities)
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// GetActivity
// ---------------------------------------------------------------------------

func (s *ActivityServer) GetActivity(
	ctx context.Context,
	req *v1.GetActivityRequest,
) (*v1.GetActivityResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}

	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument,
			"invalid activity id")
	}

	activity, err := s.Activities.GetActivity(ctx, domain.ActivityID(id))
	if err != nil {
		return nil, notFoundErr(err)
	}

	// Authorisation: the activity must belong to the authenticated user.
	// Future RBAC (admin override, follower sharing) plugs in here.
	if activity.UserID != userID {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound,
			"not found")
	}
	if activity.IsDeleted() {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound,
			"not found")
	}

	sources, err := s.Activities.ListSourcesForActivity(ctx, activity.ID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal,
			fmt.Errorf("list sources: %w", err))
	}

	return &v1.GetActivityResponse{
		Activity: s.withMapImage(ctx, withGear(activityToProto(activity, sources), activity, s.gearNames(ctx, activity)), activity),
	}, nil
}

// ---------------------------------------------------------------------------
// UpdateActivity
//
// Translates the field_mask into a domain.ActivityPatch for activity.Editor,
// which owns the edit rules, the re-merge, federation and the live event.
// ---------------------------------------------------------------------------

func (s *ActivityServer) UpdateActivity(
	ctx context.Context,
	req *v1.UpdateActivityRequest,
) (*v1.UpdateActivityResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid activity id")
	}
	if s.Editor == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "activity editing is not available")
	}
	patch, err := activityPatchFromProto(req)
	if err != nil {
		return nil, err
	}
	updated, err := s.Editor.Update(ctx, userID, domain.ActivityID(id), patch)
	if err != nil {
		return nil, activityEditError(err)
	}
	sources, err := s.Activities.ListSourcesForActivity(ctx, updated.ID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal,
			fmt.Errorf("list sources: %w", err))
	}
	return &v1.UpdateActivityResponse{
		Activity: s.withMapImage(ctx, withGear(activityToProto(updated, sources), updated, s.gearNames(ctx, updated)), updated),
	}, nil
}

// activityUpdateLoad resolves the caller and loads the activity they own.
func (s *ActivityServer) activityUpdateLoad(ctx context.Context, rawID string) (domain.Activity, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return domain.Activity{}, err
	}

	id, err := uuid.Parse(rawID)
	if err != nil {
		return domain.Activity{}, connectrpc.NewError(connectrpc.CodeInvalidArgument,
			"invalid activity id")
	}
	activityID := domain.ActivityID(id)

	activity, err := s.Activities.GetActivity(ctx, activityID)
	if err != nil {
		return domain.Activity{}, notFoundErr(err)
	}
	if activity.UserID != userID || activity.IsDeleted() {
		return domain.Activity{}, connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	}
	return activity, nil
}

// activityEditError maps activity.Editor errors to Connect codes.
func activityEditError(err error) error {
	var notEditable *activity.FieldNotEditableError
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	case errors.Is(err, activity.ErrEmptyPatch):
		return connectrpc.NewError(connectrpc.CodeInvalidArgument, "field_mask must list at least one field to update")
	case errors.As(err, &notEditable):
		return connectError(connectrpc.CodeInvalidArgument, notEditable)
	case errors.Is(err, activity.ErrGearUnavailable):
		return connectrpc.NewError(connectrpc.CodeUnavailable, "gear is not available on this instance")
	case errors.Is(err, activity.ErrGearNotFound):
		return connectrpc.NewError(connectrpc.CodeNotFound, "gear not found")
	}
	return connectError(connectrpc.CodeInternal, err)
}

// activityPatchFromProto decodes the field_mask entries; later entries win.
func activityPatchFromProto(m *v1.UpdateActivityRequest) (domain.ActivityPatch, error) {
	var p domain.ActivityPatch
	for _, f := range m.GetFieldMask() {
		if err := applyPatchField(&p, m, f); err != nil {
			return domain.ActivityPatch{}, err
		}
	}
	return p, nil
}

func applyPatchField(p *domain.ActivityPatch, m *v1.UpdateActivityRequest, f string) error {
	switch f {
	case "title":
		p.Title = new(m.GetTitle())
	case "description":
		p.Description = new(m.GetDescription())
	case "tags":
		p.Tags = new(m.GetTags())
	case "privacy":
		pr, ok := privacyFromProto(m.GetPrivacy())
		if !ok {
			return connectrpc.NewError(connectrpc.CodeInvalidArgument,
				"privacy must be one of private|followers|public")
		}
		p.Privacy = &pr
	case "primary_stream_source_id":
		// Rejected by the editor whatever the value.
		p.PrimaryStreamSourceID = &domain.SourceID{}
	case "gear_id":
		g := domain.GearEdit{}
		if raw := m.GetGearId(); raw != "" {
			gid, err := uuid.Parse(raw)
			if err != nil {
				return connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid gear_id")
			}
			g.GearID = new(domain.GearID(gid))
		}
		p.Gear = &g
	case "reset_gear":
		p.Gear = &domain.GearEdit{Reset: true}
	default:
		set, ok := overlayPatchSetters[f]
		if !ok {
			return connectError(connectrpc.CodeInvalidArgument,
				fmt.Errorf("unknown field %q in field_mask", f))
		}
		set(&p.Overlay, m)
	}
	return nil
}

// overlayPatchSetters maps overlay-backed field_mask entries to their patch setters.
var overlayPatchSetters = map[string]func(*domain.OverlayPatch, *v1.UpdateActivityRequest){
	"type": func(o *domain.OverlayPatch, m *v1.UpdateActivityRequest) {
		o.Type = &domain.Clearable[domain.ActivityType]{}
		if t, ok := activityTypeFromProto(m.GetType()); ok {
			o.Type.Value = &t // UNSPECIFIED clears → revert to merged
		}
	},
	"discipline": func(o *domain.OverlayPatch, m *v1.UpdateActivityRequest) {
		o.Discipline = &domain.Clearable[domain.Discipline]{}
		if d, ok := disciplineFromProto(m.GetDiscipline()); ok {
			o.Discipline.Value = &d
		}
	},
	"is_virtual": func(o *domain.OverlayPatch, m *v1.UpdateActivityRequest) { o.IsVirtual = new(m.GetIsVirtual()) },
	"is_commute": func(o *domain.OverlayPatch, m *v1.UpdateActivityRequest) { o.IsCommute = new(m.GetIsCommute()) },
	"is_race":    func(o *domain.OverlayPatch, m *v1.UpdateActivityRequest) { o.IsRace = new(m.GetIsRace()) },
	"is_ebike":   func(o *domain.OverlayPatch, m *v1.UpdateActivityRequest) { o.IsEbike = new(m.GetIsEbike()) },
	"custom_subtype": func(o *domain.OverlayPatch, m *v1.UpdateActivityRequest) {
		o.CustomSubtype = &domain.Clearable[string]{}
		if cs := m.GetCustomSubtype(); cs != "" {
			o.CustomSubtype.Value = &cs
		}
	},
	"distance_m": func(o *domain.OverlayPatch, m *v1.UpdateActivityRequest) {
		o.DistanceM = clearableMetric(m.GetDistanceM())
	},
	"elevation_gain_m": func(o *domain.OverlayPatch, m *v1.UpdateActivityRequest) {
		o.ElevationGainM = clearableMetric(m.GetElevationGainM())
	},
	"moving_duration_s": func(o *domain.OverlayPatch, m *v1.UpdateActivityRequest) {
		o.MovingDuration = &domain.Clearable[time.Duration]{}
		if v := m.GetMovingDurationS(); v >= 0 {
			o.MovingDuration.Value = new(time.Duration(v) * time.Second)
		}
	},
}

// clearableMetric is a summary-metric edit; negative clears → revert to merged.
func clearableMetric(v float64) *domain.Clearable[float64] {
	if v < 0 {
		return &domain.Clearable[float64]{}
	}
	return &domain.Clearable[float64]{Value: &v}
}

// ---------------------------------------------------------------------------
// GetActivityStream
//
// Reads time-series samples for a single source. The repo chooses the
// resolution: raw, 5s-CAgg or 30s-CAgg based on the request's
// max_resolution_hz hint. When 0, we default to 5s — the right tradeoff
// for typical activity-page rendering.
// ---------------------------------------------------------------------------

func (s *ActivityServer) GetActivityStream(
	ctx context.Context,
	req *v1.GetActivityStreamRequest,
) (*v1.GetActivityStreamResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}

	sourceUUID, err := uuid.Parse(req.GetActivitySourceId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument,
			"invalid activity_source_id")
	}
	sourceID := domain.SourceID(sourceUUID)

	// Ownership check via the source's parent activity. We don't have a
	// direct user_id on ActivitySource, so we look up the source then
	// its activity.
	src, err := s.Activities.GetSource(ctx, sourceID)
	if err != nil {
		return nil, notFoundErr(err)
	}
	activity, err := s.Activities.GetActivity(ctx, src.ActivityID)
	if err != nil {
		return nil, notFoundErr(err)
	}
	if activity.UserID != userID {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound,
			"not found")
	}

	channels := channelsFromProto(req.GetChannels())
	stream, err := s.Streams.QueryStream(ctx, domain.StreamQuery{
		ActivitySourceID: sourceID,
		Channels:         channels,
		Resolution:       chooseResolution(req.GetMaxResolutionHz()),
	})
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal,
			fmt.Errorf("query stream: %w", err))
	}

	// The repository treats Channels as advisory; enforce the subset here.
	return &v1.GetActivityStreamResponse{
		Stream: streamToProto(stream.KeepChannels(channels)),
	}, nil
}

// ---------------------------------------------------------------------------
// ListBestEfforts
//
// Returns every sliding-window peak computed for the activity, optionally
// filtered to a subset of BestEffortMetric values. Activity ownership is
// enforced via the parent activity's user_id.
// ---------------------------------------------------------------------------

func (s *ActivityServer) ListBestEfforts(
	ctx context.Context,
	req *v1.ListBestEffortsRequest,
) (*v1.ListBestEffortsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}

	id, err := uuid.Parse(req.GetActivityId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument,
			"invalid activity_id")
	}
	activityID := domain.ActivityID(id)

	// Ownership check — load the activity to verify the caller owns it.
	activity, err := s.Activities.GetActivity(ctx, activityID)
	if err != nil {
		return nil, notFoundErr(err)
	}
	if activity.UserID != userID || activity.IsDeleted() {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	}

	efforts, err := s.BestEfforts.ListForActivity(ctx, activityID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal,
			fmt.Errorf("list best efforts: %w", err))
	}

	// Optional metric filter. UNSPECIFIED in the input list is dropped;
	// an empty post-filter set means "no filter".
	allowed := metricFilter(req.GetMetrics())

	out := &v1.ListBestEffortsResponse{
		Efforts: make([]*v1.BestEffort, 0, len(efforts)),
	}
	for _, e := range efforts {
		if allowed != nil {
			if _, ok := allowed[e.Metric]; !ok {
				continue
			}
		}
		out.Efforts = append(out.Efforts, bestEffortToProto(e))
	}
	return out, nil
}

// metricFilter converts the proto repeated-enum filter into a domain set.
// Returns nil when no filter was requested.
func metricFilter(ms []v1.BestEffortMetric) map[domain.BestEffortMetric]struct{} {
	if len(ms) == 0 {
		return nil
	}
	out := make(map[domain.BestEffortMetric]struct{}, len(ms))
	for _, m := range ms {
		if dm, ok := bestEffortMetricFromProto(m); ok {
			out[dm] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// Helpers local to this file
// ---------------------------------------------------------------------------

// windowFromRequest maps the proto TimeRange to (start, end). An empty
// range defaults to "last 90 days" so the frontend feed has something
// to show on a fresh login.
func windowFromRequest(tr *v1.TimeRange) (start, end time.Time) {
	now := time.Now().UTC()
	if tr == nil || (tr.GetFrom() == nil && tr.GetTo() == nil) {
		return now.Add(-defaultListWindow), now
	}
	if s := tr.GetFrom(); s != nil {
		start = s.AsTime()
	} else {
		start = time.Unix(0, 0)
	}
	if e := tr.GetTo(); e != nil {
		end = e.AsTime()
	} else {
		end = now
	}
	return start, end
}

// chooseResolution picks a CAgg resolution from the request's hint. The
// hint is a target sample rate in Hz; storage offers 1Hz raw, 0.2Hz 5s,
// 0.033Hz 30s. We pick the *coarsest* resolution that meets the hint —
// fewer rows over the wire is the goal.
func chooseResolution(maxHz float64) domain.StreamResolution {
	switch {
	case maxHz <= 0:
		return domain.StreamResolution5s
	case maxHz >= 1.0:
		return domain.StreamResolutionRaw
	case maxHz >= 0.2:
		return domain.StreamResolution5s
	default:
		return domain.StreamResolution30s
	}
}

// reverseActivities reverses the slice in place. Used to flip the repo's
// ASC order to the proto-default DESC.
func reverseActivities(xs []*v1.Activity) {
	for i, j := 0, len(xs)-1; i < j; i, j = i+1, j-1 {
		xs[i], xs[j] = xs[j], xs[i]
	}
}
