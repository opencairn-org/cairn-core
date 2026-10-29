package segment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Authoring creates, edits and deletes Cairn-native segments and re-runs
// matching on request. Match may be nil (rematch then reports Unavailable).
type Authoring struct {
	Segments   port.SegmentRepo
	Activities port.ActivityRepo
	Match      *MatchSegmentsForActivity
	Now        func() time.Time
	NewID      func() uuid.UUID
}

func (a *Authoring) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now().UTC()
}

func (a *Authoring) newID() uuid.UUID {
	if a.NewID != nil {
		return a.NewID()
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}

func invalid(err error) error { return port.NewActionError(port.ActionErrorInvalid, err.Error()) }

func internalErr(op string, err error) error {
	return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("%s: %v", op, err))
}

// Create stores a native segment owned by the caller.
func (a *Authoring) Create(ctx context.Context, owner domain.UserID, in domain.NativeSegmentInput) (domain.Segment, error) {
	seg, err := domain.NewNativeSegment(domain.SegmentID(a.newID()), owner, in, a.now())
	if err != nil {
		return domain.Segment{}, invalid(err)
	}
	if err := a.Segments.SaveSegment(ctx, seg); err != nil {
		return domain.Segment{}, internalErr("save segment", err)
	}
	return seg, nil
}

// Editable loads a segment the caller may edit: their own native segment,
// or (for the starred flag only) a mirror of one of their accounts.
func (a *Authoring) editable(ctx context.Context, userID domain.UserID, id domain.SegmentID) (domain.Segment, error) {
	seg, err := a.Segments.GetSegment(ctx, id)
	if err != nil {
		return domain.Segment{}, port.NewActionError(port.ActionErrorNotFound, "segment not found")
	}
	owns := false
	if seg.ExternalAccountID != nil {
		owns, _ = a.Segments.ViewerOwnsAccount(ctx, userID, *seg.ExternalAccountID)
	}
	if seg.Source == domain.SegmentSourceNative && (seg.OwnerUserID == nil || *seg.OwnerUserID != userID) {
		if seg.Scope == domain.SegmentScopeInstance {
			return domain.Segment{}, port.NewActionError(port.ActionErrorForbidden, "only the owner can edit this segment")
		}
		return domain.Segment{}, port.NewActionError(port.ActionErrorNotFound, "segment not found")
	}
	if seg.Source == domain.SegmentSourceExternal && !owns {
		return domain.Segment{}, port.NewActionError(port.ActionErrorNotFound, "segment not found")
	}
	return seg, nil
}

// Update applies a patch; the polyline is locked once efforts exist.
func (a *Authoring) Update(ctx context.Context, userID domain.UserID, id domain.SegmentID, p domain.SegmentPatch) (domain.Segment, error) {
	seg, err := a.editable(ctx, userID, id)
	if err != nil {
		return domain.Segment{}, err
	}
	hasEfforts := false
	if p.Polyline != nil {
		n, err := a.Segments.CountEffortsForSegment(ctx, id)
		if err != nil {
			return domain.Segment{}, internalErr("count efforts", err)
		}
		hasEfforts = n > 0
	}
	if err := seg.ApplyPatch(p, hasEfforts, a.now()); err != nil {
		if errors.Is(err, domain.ErrSegmentPolylineLocked) || errors.Is(err, domain.ErrSegmentNotEditable) {
			return domain.Segment{}, port.NewActionError(port.ActionErrorPrecondition, err.Error())
		}
		return domain.Segment{}, invalid(err)
	}
	if err := a.Segments.SaveSegment(ctx, seg); err != nil {
		return domain.Segment{}, internalErr("save segment", err)
	}
	return seg, nil
}

// Delete removes the caller's native segment and (by cascade) its efforts.
func (a *Authoring) Delete(ctx context.Context, userID domain.UserID, id domain.SegmentID, keepEfforts bool) error {
	if keepEfforts {
		return port.NewActionError(port.ActionErrorInvalid, domain.ErrSegmentKeepEfforts.Error())
	}
	seg, err := a.editable(ctx, userID, id)
	if err != nil {
		return err
	}
	if seg.Source != domain.SegmentSourceNative {
		return port.NewActionError(port.ActionErrorPrecondition, "provider-mirrored segments cannot be deleted here")
	}
	if err := a.Segments.DeleteSegment(ctx, id); err != nil {
		return internalErr("delete segment", err)
	}
	return nil
}

// RematchActivity re-runs matching for every source of the caller's
// activity against all eligible segments (the matcher always evaluates the
// full candidate set, so segment_ids on the wire is accepted but not needed).
func (a *Authoring) RematchActivity(ctx context.Context, userID domain.UserID, activityID domain.ActivityID) (sources, efforts int, err error) {
	if a.Match == nil {
		return 0, 0, port.NewActionError(port.ActionErrorUnavailable, "segment matching is not available on this instance")
	}
	act, err := a.Activities.GetActivity(ctx, activityID)
	if err != nil || act.UserID != userID || act.IsDeleted() {
		return 0, 0, port.NewActionError(port.ActionErrorNotFound, "activity not found")
	}
	srcs, err := a.Activities.ListSourcesForActivity(ctx, activityID)
	if err != nil {
		return 0, 0, internalErr("list sources", err)
	}
	for _, s := range srcs {
		res, err := a.Match.Execute(ctx, Input{ActivitySourceID: s.ID})
		if err != nil {
			return sources, efforts, internalErr("match source "+s.ID.String(), err)
		}
		sources++
		efforts += res.EffortsWritten
	}
	return sources, efforts, nil
}

// RematchSegment re-runs matching over every source of the caller's
// activities of the segment's type (within the optional time window). It is
// meant to run in the background; single-source failures are counted.
func (a *Authoring) RematchSegment(ctx context.Context, userID domain.UserID, id domain.SegmentID, from, to time.Time) (sources, failed int, err error) {
	if a.Match == nil {
		return 0, 0, port.NewActionError(port.ActionErrorUnavailable, "segment matching is not available on this instance")
	}
	seg, err := a.Segments.GetSegment(ctx, id)
	if err != nil {
		return 0, 0, port.NewActionError(port.ActionErrorNotFound, "segment not found")
	}
	const page = 200
	for offset := 0; ; offset += page {
		acts, _, err := a.Activities.ListActivitiesFiltered(ctx, userID, port.ActivityListFilter{
			Type: string(seg.ActivityType), From: from, To: to, Limit: page, Offset: offset,
		})
		if err != nil {
			return sources, failed, internalErr("list activities", err)
		}
		for _, act := range acts {
			n, f := a.rematchSources(ctx, act.ID)
			sources, failed = sources+n, failed+f
		}
		if len(acts) < page {
			return sources, failed, nil
		}
	}
}

func (a *Authoring) rematchSources(ctx context.Context, id domain.ActivityID) (sources, failed int) {
	srcs, err := a.Activities.ListSourcesForActivity(ctx, id)
	if err != nil {
		return 0, 1
	}
	for _, s := range srcs {
		sources++
		if _, err := a.Match.Execute(ctx, Input{ActivitySourceID: s.ID}); err != nil {
			failed++
		}
	}
	return sources, failed
}
