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

// sourceActionStore reads activities with their sources and detaches sources.
type sourceActionStore interface {
	port.ActivityAggregateReader
	port.SourceWriter
}

// SourceActions are the owner-initiated mutations of an activity's source set
// that both the REST and the Connect surface expose: detach one source, or
// delete the whole activity. Shared here so the two surfaces cannot drift.
type SourceActions struct {
	activities sourceActionStore
	denylist   port.SourceDenylistRepo // optional; nil → detaches are not durable against re-push
	recompute  *RecomputeActivityFromSources
	federation port.FederationPublisher // optional; nil → no remote retraction
	// Events receives live events for the owner's open streams (optional).
	Events port.ActivityEventSink
	// FollowUps re-merges and runs the follow-ups an edit owes (optional;
	// nil → recompute only).
	FollowUps Remerger
	// Tx and Lock make a detach or delete one atomic write that holds the
	// owner's regrouping lock (optional; nil → statement by statement).
	Tx   port.TxManager
	Lock port.UserLock
	now  func() time.Time
}

// Remerger re-derives an activity after an edit and runs the follow-ups it
// owes; pipeline.Pipeline implements it.
type Remerger interface {
	Remerge(ctx context.Context, id domain.ActivityID) (RecomputeResult, error)
}

// NewSourceActions wires the use case; denylist and federation may be nil.
func NewSourceActions(
	activities sourceActionStore,
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

// SourceOf loads one source of the caller's activity. withDeleted admits a
// soft-deleted activity (its parsed payload stays viewable).
func (uc *SourceActions) SourceOf(ctx context.Context, userID domain.UserID, activityID domain.ActivityID, sourceID domain.SourceID, withDeleted bool) (domain.ActivitySource, error) {
	if !withDeleted {
		_, src, err := uc.OwnedSource(ctx, userID, activityID, sourceID)
		return src, err
	}
	act, err := uc.activities.GetActivity(ctx, activityID)
	if err != nil {
		return domain.ActivitySource{}, err
	}
	if act.UserID != userID {
		return domain.ActivitySource{}, domain.ErrNotFound
	}
	src, err := uc.activities.GetSource(ctx, sourceID)
	if err != nil {
		return domain.ActivitySource{}, err
	}
	if src.ActivityID != activityID {
		return domain.ActivitySource{}, ErrSourceNotOnActivity
	}
	return src, nil
}

// ParsedRaw is the stored normalized payload of a source loaded via SourceOf.
func (uc *SourceActions) ParsedRaw(ctx context.Context, src domain.ActivitySource) ([]byte, error) {
	return uc.activities.GetSourceParsedRaw(ctx, src.ID)
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
	if err := uc.write(ctx, userID, func(ctx context.Context) error {
		if err := uc.activities.DetachSource(ctx, sourceID, reason, uc.now()); err != nil {
			return fmt.Errorf("detach source: %w", err)
		}
		return uc.deny(ctx, userID, src, reason)
	}); err != nil {
		return DetachResult{}, err
	}
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
	if err := uc.write(ctx, userID, func(ctx context.Context) error {
		sources, err := uc.activities.ListSourcesForActivity(ctx, activityID)
		if err != nil {
			return fmt.Errorf("list sources: %w", err)
		}
		for _, src := range sources {
			if err := uc.activities.DetachSource(ctx, src.ID, DetachReasonDelete, uc.now()); err != nil {
				return fmt.Errorf("detach source %s: %w", src.ID, err)
			}
			if err := uc.deny(ctx, userID, src, DetachReasonDelete); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
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

// write runs fn in one transaction holding the owner's regrouping lock, when wired.
func (uc *SourceActions) write(ctx context.Context, userID domain.UserID, fn func(context.Context) error) error {
	if uc.Tx == nil {
		return fn(ctx)
	}
	return uc.Tx.InTx(ctx, func(ctx context.Context) error {
		if uc.Lock != nil {
			if err := uc.Lock.LockUser(ctx, userID); err != nil {
				return err
			}
		}
		return fn(ctx)
	})
}

// deny records the source identity on the user's denylist. It is part of the
// detach: without it the next provider sync would resurrect the source.
func (uc *SourceActions) deny(ctx context.Context, userID domain.UserID, src domain.ActivitySource, reason string) error {
	if uc.denylist == nil {
		return nil
	}
	if err := uc.denylist.Add(ctx, domain.SourceDenylistEntry{
		UserID:            userID,
		Provider:          src.Provider,
		ExternalAccountID: src.ExternalAccountID,
		ExternalID:        src.ExternalID,
		Reason:            reason,
	}); err != nil {
		return fmt.Errorf("denylist source: %w", err)
	}
	return nil
}

// remerge re-runs the merge over the remaining sources and federates a
// resulting soft-delete.
func (uc *SourceActions) remerge(ctx context.Context, act domain.Activity) DetachResult {
	if uc.FollowUps != nil {
		result, err := uc.FollowUps.Remerge(ctx, act.ID)
		if err != nil {
			return DetachResult{RecomputeErr: err}
		}
		return DetachResult{Recomputed: true, SoftDeleted: result.SoftDeleted, SourceCount: len(result.SourceIDs)}
	}
	result, err := uc.recompute.Execute(ctx, act.ID)
	if err != nil {
		return DetachResult{RecomputeErr: err}
	}
	if result.SoftDeleted && uc.federation != nil {
		uc.federation.PublishDelete(ctx, act.UserID, act.ID)
	}
	return DetachResult{Recomputed: true, SoftDeleted: result.SoftDeleted, SourceCount: len(result.SourceIDs)}
}
