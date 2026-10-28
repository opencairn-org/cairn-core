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
	"fmt"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	gearuc "github.com/opencairn-org/cairn-core/internal/usecase/gear"
	matchuc "github.com/opencairn-org/cairn-core/internal/usecase/match"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// ActivityServer implements cairnv1connect.ActivityServiceHandler. The
// embedded Unimplemented base returns CodeUnimplemented for every RPC
// this struct doesn't override.
type ActivityServer struct {
	cairnv1connect.UnimplementedActivityServiceHandler

	Activities  port.ActivityRepo
	Streams     port.StreamRepo
	BestEfforts port.BestEffortRepo

	// Recompute re-runs the merge engine over an activity's sources after a
	// user edit, so the persisted row stays canonical and preserveUserEdits
	// keeps the edit across future source imports.
	Recompute *activity.RecomputeActivityFromSources

	// ClassOverrides persists the user's classification overlay (type/flags),
	// applied after the merge by Recompute so the edit survives re-derivation.
	ClassOverrides port.ClassificationOverrideRepo

	// Federation announces visibility changes to remote followers (nil when
	// federation is off). Lets making an activity public after import federate
	// it, since the ingest-time publish only fires for public-at-import.
	Federation port.FederationPublisher

	// Gear backs the gear_id edit (ownership check, type default on reset)
	// and the gear names on the wire. Nil → gear edits rejected.
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

	// Content signs photo/map/export URLs; Attachments lists photos;
	// Visibility gates non-owner reads; Exports renders interchange files;
	// MapETag derives Activity.map_image_etag. Nil → the RPC is Unavailable
	// (or the field stays empty).
	Content     port.ContentSigner
	Attachments port.AttachmentRepo
	Visibility  *view.Visibility
	Exports     *view.Exports
	Searches    *view.Searches
	MapETag     func(domain.Activity) string
}

// Compile-time interface check.
var _ cairnv1connect.ActivityServiceHandler = (*ActivityServer)(nil)

// NewActivityServer wires the server with its dependencies. Callers pass
// the concrete repos coming out of wire.go.
func NewActivityServer(
	activities port.ActivityRepo,
	streams port.StreamRepo,
	bestEfforts port.BestEffortRepo,
	recompute *activity.RecomputeActivityFromSources,
	classOverrides port.ClassificationOverrideRepo,
	federation port.FederationPublisher,
	gear *gearuc.Service,
) *ActivityServer {
	return &ActivityServer{
		Activities:     activities,
		Streams:        streams,
		BestEfforts:    bestEfforts,
		Recompute:      recompute,
		ClassOverrides: classOverrides,
		Federation:     federation,
		Gear:           gear,
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
		out.Activities = append(out.Activities, s.withMapImage(ctx, withGear(activityToProto(a, sources), a, names), a, false))
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
		Activity: s.withMapImage(ctx, withGear(activityToProto(activity, sources), activity, s.gearNames(ctx, activity)), activity, false),
	}, nil
}

// ---------------------------------------------------------------------------
// UpdateActivity
//
// Applies user edits to an activity and re-runs the merge so the edit
// survives future source imports (the merge engine's preserveUserEdits
// owns Title/Description/Tags/Privacy once an activity exists).
//
// Only field_mask entries are touched. v1 accepts the merge-preserved
// user-editable fields; the source-driven classification fields (type,
// discipline, the is_* flags, custom_subtype, primary_stream_source_id)
// are exposed on the proto for forward-compatibility but rejected here —
// the merge engine would overwrite them on the next import. Making those
// user-overridable needs per-field user-edit tracking in the merge engine
// (a future change), so we fail loudly rather than silently revert.
// ---------------------------------------------------------------------------

func (s *ActivityServer) UpdateActivity(
	ctx context.Context,
	req *v1.UpdateActivityRequest,
) (*v1.UpdateActivityResponse, error) {
	activity, err := s.activityUpdateLoad(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	activityID := activity.ID

	mask := req.GetFieldMask()
	if len(mask) == 0 {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument,
			"field_mask must list at least one field to update")
	}

	// Capture the pre-edit privacy so a public⇄non-public transition can be
	// federated after the save.
	prevPrivacy := activity.Privacy
	e := &activityUpdateEdit{s: s, ctx: ctx, msg: req, activity: &activity}
	for _, f := range mask {
		if err := e.applyField(f); err != nil {
			return nil, err
		}
	}

	if err := e.persist(); err != nil {
		return nil, err
	}

	updated, err := s.Activities.GetActivity(ctx, activityID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal,
			fmt.Errorf("reload activity: %w", err))
	}

	if e.privacyTouched {
		s.activityUpdateFederate(ctx, prevPrivacy, updated)
	}

	sources, err := s.Activities.ListSourcesForActivity(ctx, activityID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal,
			fmt.Errorf("list sources: %w", err))
	}

	return &v1.UpdateActivityResponse{
		Activity: s.withMapImage(ctx, withGear(activityToProto(updated, sources), updated, s.gearNames(ctx, updated)), updated, false),
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

// activityUpdateFederate federates a visibility transition: making an activity
// public announces it to remote followers (the ingest-time publish only covers
// public-at-import); taking it back out of public retracts it. Best-effort,
// never fails the edit.
func (s *ActivityServer) activityUpdateFederate(ctx context.Context, prevPrivacy domain.ActivityPrivacy, updated domain.Activity) {
	if s.Federation == nil || prevPrivacy == updated.Privacy {
		return
	}
	switch {
	case updated.Privacy == domain.PrivacyPublic:
		s.Federation.PublishCreate(ctx, updated)
	case prevPrivacy == domain.PrivacyPublic:
		s.Federation.PublishDelete(ctx, updated.UserID, updated.ID)
	}
}

// activityUpdateEdit accumulates the field_mask edits of one UpdateActivity call.
type activityUpdateEdit struct {
	s              *ActivityServer
	ctx            context.Context
	msg            *v1.UpdateActivityRequest
	activity       *domain.Activity
	privacyTouched bool

	// Classification edits (type/discipline/flags) are stored as a user overlay
	// applied AFTER the merge by Recompute, so they survive future source
	// imports. We start from the existing overlay and mutate only masked fields.
	classOverride *domain.ClassificationOverride
	classTouched  bool
}

// applyField applies one field_mask entry.
func (e *activityUpdateEdit) applyField(f string) error {
	switch f {
	case "title":
		e.activity.Title = e.msg.GetTitle()
	case "description":
		e.activity.Description = e.msg.GetDescription()
	case "tags":
		// Present-in-mask replaces the whole set (empty clears it).
		e.activity.Tags = e.msg.GetTags()
	case "privacy":
		p, ok := privacyFromProto(e.msg.GetPrivacy())
		if !ok {
			return connectrpc.NewError(connectrpc.CodeInvalidArgument,
				"privacy must be one of private|followers|public")
		}
		e.activity.Privacy = p
		e.privacyTouched = true
	case "primary_stream_source_id":
		// Merge-managed (FieldGroupGPSTrack); changed via a gps_track field
		// pin, not a direct edit.
		return activityUpdateRejectClass(f)
	case "gear_id":
		return e.applyGearID()
	case "reset_gear":
		return e.applyGearReset()
	default:
		set, ok := activityUpdateClassSetters[f]
		if !ok {
			return connectError(connectrpc.CodeInvalidArgument,
				fmt.Errorf("unknown field %q in field_mask", f))
		}
		return e.applyClassField(f, set)
	}
	return nil
}

// applyGearID attaches the caller's gear ("" = no gear) and marks the choice
// as the user's so recompute leaves it alone.
func (e *activityUpdateEdit) applyGearID() error {
	if e.s.Gear == nil {
		return connectrpc.NewError(connectrpc.CodeUnavailable, "gear is not available on this instance")
	}
	raw := e.msg.GetGearId()
	if raw == "" {
		e.activity.GearID = nil
	} else {
		id, err := uuid.Parse(raw)
		if err != nil {
			return connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid gear_id")
		}
		g, err := e.s.Gear.Get(e.ctx, e.activity.UserID, domain.GearID(id))
		if err != nil {
			return connectrpc.NewError(connectrpc.CodeNotFound, "gear not found")
		}
		e.activity.GearID = &g.ID
	}
	e.activity.GearSetBy = domain.GearSetByUser
	return nil
}

// applyGearReset hands the choice back to the merge. Recompute re-derives it
// from the sources; a provider-less activity (manual, upload) gets the type
// default again here, since recompute applies that only on the first merge
// and otherwise keeps a default-applied gear.
func (e *activityUpdateEdit) applyGearReset() error {
	if e.s.Gear == nil {
		return connectrpc.NewError(connectrpc.CodeUnavailable, "gear is not available on this instance")
	}
	e.activity.GearID = nil
	e.activity.GearSetBy = domain.GearSetByMerge
	sources, err := e.s.Activities.ListSourcesForActivity(e.ctx, e.activity.ID)
	if err != nil {
		return connectError(connectrpc.CodeInternal, fmt.Errorf("list sources: %w", err))
	}
	for _, src := range sources {
		if src.ExternalAccountID != nil {
			return nil
		}
	}
	gid, ok, err := e.s.Gear.DefaultFor(e.ctx, e.activity.UserID, e.activity.Type)
	if err != nil {
		return connectError(connectrpc.CodeInternal, fmt.Errorf("gear default: %w", err))
	}
	if ok {
		e.activity.GearID = &gid
	}
	return nil
}

// applyClassField loads the overlay on first use and applies set to it.
func (e *activityUpdateEdit) applyClassField(f string, set func(*domain.ClassificationOverride, *v1.UpdateActivityRequest)) error {
	if e.s.ClassOverrides == nil {
		return activityUpdateRejectClass(f)
	}
	if e.classOverride == nil {
		existing, err := e.s.ClassOverrides.Get(e.ctx, e.activity.ID)
		if err != nil {
			return connectError(connectrpc.CodeInternal, err)
		}
		e.classOverride = &existing
	}
	e.classTouched = true
	set(e.classOverride, e.msg)
	return nil
}

// persist saves the overlay and activity, then re-merges from all sources.
func (e *activityUpdateEdit) persist() error {
	if e.classTouched {
		if err := e.s.ClassOverrides.Set(e.ctx, *e.classOverride); err != nil {
			return connectError(connectrpc.CodeInternal,
				fmt.Errorf("save classification override: %w", err))
		}
	}

	if err := e.s.Activities.SaveActivity(e.ctx, *e.activity); err != nil {
		return connectError(connectrpc.CodeInternal,
			fmt.Errorf("save activity: %w", err))
	}

	// Re-merge from all sources. preserveUserEdits copies the just-saved
	// user fields onto the merged result, so the edit sticks.
	if _, err := e.s.Recompute.Execute(e.ctx, e.activity.ID); err != nil {
		return connectError(connectrpc.CodeInternal,
			fmt.Errorf("recompute activity: %w", err))
	}
	return nil
}

func activityUpdateRejectClass(f string) error {
	return connectError(connectrpc.CodeInvalidArgument,
		fmt.Errorf("field %q is not editable on this instance", f))
}

// activityUpdateClassSetters maps overlay-backed field_mask entries to their setters.
var activityUpdateClassSetters = map[string]func(*domain.ClassificationOverride, *v1.UpdateActivityRequest){
	"type": func(co *domain.ClassificationOverride, m *v1.UpdateActivityRequest) {
		if t, ok := activityTypeFromProto(m.GetType()); ok {
			co.Type = &t
		} else {
			co.Type = nil // UNSPECIFIED clears the override → revert to merged
		}
	},
	"discipline": func(co *domain.ClassificationOverride, m *v1.UpdateActivityRequest) {
		if d, ok := disciplineFromProto(m.GetDiscipline()); ok {
			co.Discipline = &d
		} else {
			co.Discipline = nil
		}
	},
	"is_virtual": func(co *domain.ClassificationOverride, m *v1.UpdateActivityRequest) {
		v := m.GetIsVirtual()
		co.IsVirtual = &v
	},
	"is_commute": func(co *domain.ClassificationOverride, m *v1.UpdateActivityRequest) {
		v := m.GetIsCommute()
		co.IsCommute = &v
	},
	"is_race": func(co *domain.ClassificationOverride, m *v1.UpdateActivityRequest) {
		v := m.GetIsRace()
		co.IsRace = &v
	},
	"is_ebike": func(co *domain.ClassificationOverride, m *v1.UpdateActivityRequest) {
		v := m.GetIsEbike()
		co.IsEbike = &v
	},
	"custom_subtype": func(co *domain.ClassificationOverride, m *v1.UpdateActivityRequest) {
		cs := m.GetCustomSubtype()
		if cs == "" {
			co.CustomSubtype = nil
		} else {
			co.CustomSubtype = &cs
		}
	},
	"distance_m": func(co *domain.ClassificationOverride, m *v1.UpdateActivityRequest) {
		if v := m.GetDistanceM(); v < 0 {
			co.DistanceM = nil // negative clears → revert to merged
		} else {
			co.DistanceM = &v
		}
	},
	"elevation_gain_m": func(co *domain.ClassificationOverride, m *v1.UpdateActivityRequest) {
		if v := m.GetElevationGainM(); v < 0 {
			co.ElevationGainM = nil
		} else {
			co.ElevationGainM = &v
		}
	},
	"moving_duration_s": func(co *domain.ClassificationOverride, m *v1.UpdateActivityRequest) {
		if v := m.GetMovingDurationS(); v < 0 {
			co.MovingDuration = nil
		} else {
			d := time.Duration(v) * time.Second
			co.MovingDuration = &d
		}
	},
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
