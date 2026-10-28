package connect

import (
	"context"
	"errors"
	"fmt"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	matchuc "github.com/opencairn-org/cairn-core/internal/usecase/match"
)

// ---------------------------------------------------------------------------
// Source management RPCs. Owner-only, like GetActivity; they call the same use
// cases as the REST handlers (/api/activities/{id}/sources/…, /split, /merge,
// DELETE /api/activities/{id}) so both surfaces behave identically.
// ---------------------------------------------------------------------------

// actionErr maps use-case and port.ActionError failures to Connect codes.
func actionErr(err error) error {
	var ae *port.ActionError
	switch {
	case errors.As(err, &ae):
		return connectError(actionCode(ae.Kind), err)
	case errors.Is(err, domain.ErrNotFound),
		errors.Is(err, activity.ErrSourceNotOnActivity),
		errors.Is(err, matchuc.ErrSourceNotOnActivity):
		return connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	case errors.Is(err, matchuc.ErrSingleSource),
		errors.Is(err, matchuc.ErrSameActivity):
		return connectError(connectrpc.CodeFailedPrecondition, err)
	case errors.Is(err, matchuc.ErrReclusterFailed):
		return connectError(connectrpc.CodeAborted, err)
	}
	return connectError(connectrpc.CodeInternal, err)
}

func actionCode(k port.ActionErrorKind) connectrpc.Code {
	switch k {
	case port.ActionErrorInvalid:
		return connectrpc.CodeInvalidArgument
	case port.ActionErrorNotFound:
		return connectrpc.CodeNotFound
	case port.ActionErrorPrecondition, port.ActionErrorConflict:
		return connectrpc.CodeFailedPrecondition
	case port.ActionErrorUnavailable, port.ActionErrorUpstream:
		return connectrpc.CodeUnavailable
	case port.ActionErrorQuota:
		return connectrpc.CodeResourceExhausted
	case port.ActionErrorInternal:
	}
	return connectrpc.CodeInternal
}

func parseActivityID(raw string) (domain.ActivityID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return domain.ActivityID{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid activity id")
	}
	return domain.ActivityID(id), nil
}

func parseSourceID(raw string) (domain.SourceID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return domain.SourceID{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid activity_source_id")
	}
	return domain.SourceID(id), nil
}

// ownedSource resolves the caller and loads one of their non-detached sources
// with its live activity. The source id alone identifies it on the wire.
func (s *ActivityServer) ownedSource(ctx context.Context, rawSourceID string) (domain.UserID, domain.Activity, domain.ActivitySource, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return domain.UserID{}, domain.Activity{}, domain.ActivitySource{}, err
	}
	sourceID, err := parseSourceID(rawSourceID)
	if err != nil {
		return domain.UserID{}, domain.Activity{}, domain.ActivitySource{}, err
	}
	src, err := s.Activities.GetSource(ctx, sourceID)
	if err != nil {
		return domain.UserID{}, domain.Activity{}, domain.ActivitySource{}, notFoundErr(err)
	}
	act, err := s.Activities.GetActivity(ctx, src.ActivityID)
	if err != nil {
		return domain.UserID{}, domain.Activity{}, domain.ActivitySource{}, notFoundErr(err)
	}
	if act.UserID != userID || act.IsDeleted() || src.Status == domain.SourceStatusDetached {
		return domain.UserID{}, domain.Activity{}, domain.ActivitySource{}, connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	}
	return userID, act, src, nil
}

// wireActivity reloads an activity with its sources for a response.
func (s *ActivityServer) wireActivity(ctx context.Context, id domain.ActivityID) (*v1.Activity, error) {
	act, err := s.Activities.GetActivity(ctx, id)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("reload activity: %w", err))
	}
	sources, err := s.Activities.ListSourcesForActivity(ctx, id)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("list sources: %w", err))
	}
	return withGear(activityToProto(act, sources), act, s.gearNames(ctx, act)), nil
}

// ListActivitySources returns every source of the caller's activity,
// detached ones included (status tells them apart).
func (s *ActivityServer) ListActivitySources(ctx context.Context, req *v1.ListActivitySourcesRequest) (*v1.ListActivitySourcesResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseActivityID(req.GetActivityId())
	if err != nil {
		return nil, err
	}
	act, err := s.Activities.GetActivity(ctx, id)
	if err != nil {
		return nil, notFoundErr(err)
	}
	if act.UserID != userID || act.IsDeleted() {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	}
	sources, err := s.Activities.ListAllSourcesForActivity(ctx, id)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("list sources: %w", err))
	}
	out := &v1.ListActivitySourcesResponse{Sources: make([]*v1.ActivitySource, 0, len(sources))}
	for _, src := range sources {
		out.Sources = append(out.Sources, sourceToProto(src))
	}
	return out, nil
}

// DeleteActivity soft-deletes the caller's activity: every source is detached
// and denylisted so a provider re-push cannot resurrect it.
func (s *ActivityServer) DeleteActivity(ctx context.Context, req *v1.DeleteActivityRequest) (*v1.DeleteActivityResponse, error) {
	if s.SourceActions == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "source actions are not available on this instance")
	}
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseActivityID(req.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.SourceActions.Delete(ctx, userID, id); err != nil {
		return nil, actionErr(err)
	}
	return &v1.DeleteActivityResponse{}, nil
}

// DetachSource removes a source from the merge. With keep_as_new_activity the
// source is split into its own activity instead (cannot-link + recluster),
// which is returned.
func (s *ActivityServer) DetachSource(ctx context.Context, req *v1.DetachSourceRequest) (*v1.DetachSourceResponse, error) {
	if s.SourceActions == nil || s.SourceLinks == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "source actions are not available on this instance")
	}
	userID, act, src, err := s.ownedSource(ctx, req.GetActivitySourceId())
	if err != nil {
		return nil, err
	}
	if req.GetKeepAsNewActivity() {
		fresh, err := s.SourceLinks.Split(ctx, userID, act.ID, src.ID)
		if err != nil {
			return nil, actionErr(err)
		}
		wire, err := s.wireActivity(ctx, fresh.ID)
		if err != nil {
			return nil, err
		}
		return &v1.DetachSourceResponse{NewActivity: wire}, nil
	}
	res, err := s.SourceActions.Detach(ctx, userID, act.ID, src.ID, activity.DetachReasonUser)
	if err != nil {
		return nil, actionErr(err)
	}
	if res.RecomputeErr != nil {
		// The detach is durable; report the failed re-merge like REST does.
		return nil, connectError(connectrpc.CodeAborted, fmt.Errorf("source detached but recompute failed: %w", res.RecomputeErr))
	}
	return &v1.DetachSourceResponse{}, nil
}

// LinkSourceToActivity must-links a source onto another of the caller's
// activities and re-clusters. The constraint always overrides the heuristic,
// so `force` is accepted but makes no difference.
func (s *ActivityServer) LinkSourceToActivity(ctx context.Context, req *v1.LinkSourceToActivityRequest) (*v1.LinkSourceToActivityResponse, error) {
	if s.SourceLinks == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "source actions are not available on this instance")
	}
	userID, _, src, err := s.ownedSource(ctx, req.GetActivitySourceId())
	if err != nil {
		return nil, err
	}
	target, err := parseActivityID(req.GetTargetActivityId())
	if err != nil {
		return nil, err
	}
	linked, err := s.SourceLinks.Link(ctx, userID, src.ID, target)
	if err != nil {
		return nil, actionErr(err)
	}
	wire, err := s.wireActivity(ctx, linked.ID)
	if err != nil {
		return nil, err
	}
	return &v1.LinkSourceToActivityResponse{Activity: wire}, nil
}

// ReimportSource queues a re-fetch from the provider, or a re-parse of the
// archived original when prefer_local_reparse is set.
func (s *ActivityServer) ReimportSource(ctx context.Context, req *v1.ReimportSourceRequest) (*v1.ReimportSourceResponse, error) {
	if s.Reimporter == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "re-import is not available on this instance")
	}
	userID, _, src, err := s.ownedSource(ctx, req.GetActivitySourceId())
	if err != nil {
		return nil, err
	}
	var jobID string
	if req.GetPreferLocalReparse() {
		jobID, err = s.Reimporter.Reparse(ctx, userID, src)
	} else {
		jobID, err = s.Reimporter.Refetch(ctx, userID, src)
	}
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.ReimportSourceResponse{JobId: jobID}, nil
}
