package activity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ErrSourceNotOnActivity is returned when a source id does not belong to the
// named activity — the caller may own both, but the pair is wrong.
var ErrSourceNotOnActivity = errors.New("activity: source not on this activity")

// Detach reasons recorded on the source and its denylist entry.
const (
	DetachReasonUser   = "user_detach"
	DetachReasonDelete = "user_delete"
)

// SourceActions are the owner-initiated mutations of an activity's source set
// that both the REST and the Connect surface expose: detach one source, or
// delete the whole activity. Shared here so the two surfaces cannot drift.
type SourceActions struct {
	activities port.ActivityRepo
	denylist   port.SourceDenylistRepo // optional; nil → detaches are not durable against re-push
	recompute  *RecomputeActivityFromSources
	federation port.FederationPublisher // optional; nil → no remote retraction
	// Events receives live events for the owner's open streams (optional).
	Events port.ActivityEventSink
	now    func() time.Time
}

// NewSourceActions wires the use case; denylist and federation may be nil.
func NewSourceActions(
	activities port.ActivityRepo,
	denylist port.SourceDenylistRepo,
	recompute *RecomputeActivityFromSources,
	federation port.FederationPublisher,
	now func() time.Time,
) *SourceActions {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &SourceActions{activities: activities, denylist: denylist, recompute: recompute, federation: federation, now: now}
}

// OwnedActivity loads the caller's live activity; domain.ErrNotFound hides
// other users' rows and deleted ones alike.
func (uc *SourceActions) OwnedActivity(ctx context.Context, userID domain.UserID, id domain.ActivityID) (domain.Activity, error) {
	act, err := uc.activities.GetActivity(ctx, id)
	if err != nil {
		return domain.Activity{}, err
	}
	if act.UserID != userID || act.IsDeleted() {
		return domain.Activity{}, domain.ErrNotFound
	}
	return act, nil
}

// OwnedSource loads the caller's live activity and one of its sources. A source
// of another activity (even the caller's own) yields ErrSourceNotOnActivity.
func (uc *SourceActions) OwnedSource(ctx context.Context, userID domain.UserID, activityID domain.ActivityID, sourceID domain.SourceID) (domain.Activity, domain.ActivitySource, error) {
	act, err := uc.OwnedActivity(ctx, userID, activityID)
	if err != nil {
		return domain.Activity{}, domain.ActivitySource{}, err
	}
	src, err := uc.activities.GetSource(ctx, sourceID)
	if err != nil {
		return domain.Activity{}, domain.ActivitySource{}, err
	}
	if src.ActivityID != activityID {
		return domain.Activity{}, domain.ActivitySource{}, ErrSourceNotOnActivity
	}
	return act, src, nil
}

// DetachResult reports a detach. The detach itself is durable once Detach
// returns nil; the re-merge is best-effort and reported, not failed.
type DetachResult struct {
	Recomputed   bool
	RecomputeErr error
	SoftDeleted  bool // the detached source was the last one
	SourceCount  int  // sources still contributing
}

// Detach removes one source from the merge, denylists its identity so a
// provider re-push cannot re-attach it, and re-merges the remainder (or
// soft-deletes the activity when nothing remains, retracting it from remote
// followers).
func (uc *SourceActions) Detach(ctx context.Context, userID domain.UserID, activityID domain.ActivityID, sourceID domain.SourceID, reason string) (DetachResult, error) {
	act, src, err := uc.OwnedSource(ctx, userID, activityID, sourceID)
	if err != nil {
		return DetachResult{}, err
	}
	if reason == "" {
		reason = DetachReasonUser
	}
	if err := uc.activities.DetachSource(ctx, sourceID, reason, uc.now()); err != nil {
		return DetachResult{}, fmt.Errorf("detach source: %w", err)
	}
	uc.deny(ctx, userID, src, reason)
	res := uc.remerge(ctx, act)
	uc.publish(domain.NewActivityEvent(domain.ActivityEventSourceDetached, userID, "activity.source_detached").
		For(activityID, sourceID).With("provider", src.Provider))
	if res.SoftDeleted {
		uc.publish(domain.NewActivityEvent(domain.ActivityEventActivityDeleted, userID, "activity.deleted").For(activityID, domain.SourceID{}))
	}
	return res, nil
}

// Delete soft-deletes an activity by detaching every contributing source. The
// detaches are denylisted so the next provider sync does not resurrect the
// activity; raw blobs stay in storage per lifecycle. Already-deleted or
// foreign activities are ErrNotFound.
func (uc *SourceActions) Delete(ctx context.Context, userID domain.UserID, activityID domain.ActivityID) error {
	act, err := uc.OwnedActivity(ctx, userID, activityID)
	if err != nil {
		return err
	}
	sources, err := uc.activities.ListSourcesForActivity(ctx, activityID)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}
	for _, src := range sources {
		if err := uc.activities.DetachSource(ctx, src.ID, DetachReasonDelete, uc.now()); err != nil {
			return fmt.Errorf("detach source %s: %w", src.ID, err)
		}
		uc.deny(ctx, userID, src, DetachReasonDelete)
	}
	res := uc.remerge(ctx, act)
	if res.RecomputeErr != nil {
		return fmt.Errorf("delete activity: %w", res.RecomputeErr)
	}
	if !res.SoftDeleted {
		return errors.New("delete activity: recompute left the activity alive")
	}
	uc.publish(domain.NewActivityEvent(domain.ActivityEventActivityDeleted, userID, "activity.deleted").For(activityID, domain.SourceID{}))
	return nil
}

// publish hands an event to the sink when one is wired.
func (uc *SourceActions) publish(ev domain.ActivityEvent) {
	if uc.Events != nil {
		uc.Events.Publish(ev)
	}
}

// deny records the source identity on the user's denylist; best-effort.
func (uc *SourceActions) deny(ctx context.Context, userID domain.UserID, src domain.ActivitySource, reason string) {
	if uc.denylist == nil {
		return
	}
	_ = uc.denylist.Add(ctx, domain.SourceDenylistEntry{
		UserID:            userID,
		Provider:          src.Provider,
		ExternalAccountID: src.ExternalAccountID,
		ExternalID:        src.ExternalID,
		Reason:            reason,
	})
}

// remerge re-runs the merge over the remaining sources and federates a
// resulting soft-delete.
func (uc *SourceActions) remerge(ctx context.Context, act domain.Activity) DetachResult {
	result, err := uc.recompute.Execute(ctx, act.ID)
	if err != nil {
		return DetachResult{RecomputeErr: err}
	}
	if result.SoftDeleted && uc.federation != nil {
		uc.federation.PublishDelete(ctx, act.UserID, act.ID)
	}
	return DetachResult{Recomputed: true, SoftDeleted: result.SoftDeleted, SourceCount: len(result.SourceIDs)}
}
