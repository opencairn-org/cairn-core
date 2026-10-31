package match

import (
	"context"
	"errors"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Errors of the manual link/split actions.
var (
	// ErrSingleSource: a split needs at least one other source to split from.
	ErrSingleSource = errors.New("match: activity has only one source")
	// ErrSameActivity: linking a source onto the activity it already belongs to.
	ErrSameActivity = errors.New("match: source already belongs to the target activity")
	// ErrReclusterFailed: the constraint is recorded but the re-cluster did not
	// run; the next ingest in that window applies it.
	ErrReclusterFailed = errors.New("match: constraint recorded but recluster failed")
	// ErrSourceNotOnActivity: the source does not belong to the named activity.
	ErrSourceNotOnActivity = errors.New("match: source not on this activity")
)

// Constraint reasons recorded on manual links.
const (
	ReasonUserMerge = "user_merge"
	ReasonUserSplit = "user_split"
	ReasonUserKeep  = "user_keep_both"
)

// sourceLinkStore reads activities with their sources and records match constraints.
type sourceLinkStore interface {
	port.ActivityAggregateReader
	port.ActivityMatchStore
}

// SourceLinks are the owner's manual corrections of the dedup: force two
// sources together (must-link) or apart (cannot-link), then re-cluster the
// window so the constraint takes effect. Shared by REST and Connect.
type SourceLinks struct {
	activities sourceLinkStore
	recluster  *ReclusterBucket
	// Events receives live events for the owner's open streams (optional).
	Events port.ActivityEventSink
}

// NewSourceLinks wires the use case.
func NewSourceLinks(activities sourceLinkStore, recluster *ReclusterBucket) *SourceLinks {
	return &SourceLinks{activities: activities, recluster: recluster}
}

// Link must-links src onto a representative source of the target activity and
// re-clusters. Returns the activity the source belongs to afterwards.
func (uc *SourceLinks) Link(ctx context.Context, userID domain.UserID, sourceID domain.SourceID, targetID domain.ActivityID) (domain.Activity, error) {
	src, err := uc.ownedSource(ctx, userID, sourceID)
	if err != nil {
		return domain.Activity{}, err
	}
	if src.ActivityID == targetID {
		return domain.Activity{}, ErrSameActivity
	}
	anchor, err := uc.representative(ctx, userID, targetID)
	if err != nil {
		return domain.Activity{}, err
	}
	if err := uc.constrainAndRecluster(ctx, userID, src, []domain.ActivitySource{anchor}, domain.ConstraintMustLink, ReasonUserMerge); err != nil {
		return domain.Activity{}, err
	}
	act, err := uc.activityOfSource(ctx, sourceID)
	if err == nil && uc.Events != nil {
		uc.Events.Publish(domain.NewActivityEvent(domain.ActivityEventSourceAttached, userID, "activity.source_attached").
			For(act.ID, sourceID).With("provider", src.Provider))
	}
	return act, err
}

// LinkActivities joins two of the caller's activities (must-link between a
// representative source of each) and re-clusters.
func (uc *SourceLinks) LinkActivities(ctx context.Context, userID domain.UserID, a, b domain.ActivityID) error {
	if a == b {
		return ErrSameActivity
	}
	srcA, err := uc.representative(ctx, userID, a)
	if err != nil {
		return err
	}
	srcB, err := uc.representative(ctx, userID, b)
	if err != nil {
		return err
	}
	return uc.constrainAndRecluster(ctx, userID, srcA, []domain.ActivitySource{srcB}, domain.ConstraintMustLink, ReasonUserMerge)
}

// KeepApart records the owner's "keep both" on a possible duplicate:
// cannot-link every source of a against every source of b. Both are already
// separate activities, so no re-cluster is needed.
func (uc *SourceLinks) KeepApart(ctx context.Context, userID domain.UserID, a, b domain.ActivityID) error {
	if a == b {
		return ErrSameActivity
	}
	var sides [2][]domain.ActivitySource
	for i, id := range [2]domain.ActivityID{a, b} {
		if err := uc.ownedActivity(ctx, userID, id); err != nil {
			return err
		}
		srcs, err := uc.activities.ListSourcesForActivity(ctx, id)
		if err != nil {
			return fmt.Errorf("list sources: %w", err)
		}
		sides[i] = srcs
	}
	for _, x := range sides[0] {
		for _, y := range sides[1] {
			if err := uc.activities.AddMatchConstraint(ctx, domain.MatchConstraint{
				UserID: userID, SourceA: x.ID, SourceB: y.ID, Kind: domain.ConstraintCannotLink, Reason: ReasonUserKeep,
			}); err != nil {
				return fmt.Errorf("constraint write: %w", err)
			}
		}
	}
	return nil
}

// Split peels one source off its activity: cannot-link it against every other
// source there and re-cluster. Returns the activity the source lives in now.
func (uc *SourceLinks) Split(ctx context.Context, userID domain.UserID, activityID domain.ActivityID, sourceID domain.SourceID) (domain.Activity, error) {
	if err := uc.ownedActivity(ctx, userID, activityID); err != nil {
		return domain.Activity{}, err
	}
	sources, err := uc.activities.ListSourcesForActivity(ctx, activityID)
	if err != nil {
		return domain.Activity{}, fmt.Errorf("list sources: %w", err)
	}
	var peel *domain.ActivitySource
	others := make([]domain.ActivitySource, 0, len(sources))
	for i := range sources {
		if sources[i].ID == sourceID {
			peel = &sources[i]
		} else {
			others = append(others, sources[i])
		}
	}
	if peel == nil {
		return domain.Activity{}, ErrSourceNotOnActivity
	}
	if len(others) == 0 {
		return domain.Activity{}, ErrSingleSource
	}
	if err := uc.constrainAndRecluster(ctx, userID, *peel, others, domain.ConstraintCannotLink, ReasonUserSplit); err != nil {
		return domain.Activity{}, err
	}
	return uc.activityOfSource(ctx, sourceID)
}

// constrainAndRecluster records one constraint per partner and re-clusters
// the window around src.
func (uc *SourceLinks) constrainAndRecluster(ctx context.Context, userID domain.UserID, src domain.ActivitySource, partners []domain.ActivitySource, kind domain.ConstraintKind, reason string) error {
	for _, p := range partners {
		if err := uc.activities.AddMatchConstraint(ctx, domain.MatchConstraint{
			UserID: userID, SourceA: src.ID, SourceB: p.ID, Kind: kind, Reason: reason,
		}); err != nil {
			return fmt.Errorf("constraint write: %w", err)
		}
	}
	if _, err := uc.recluster.Execute(ctx, Input{UserID: userID, Around: src.Parsed.StartTime}); err != nil {
		return fmt.Errorf("%w: %w", ErrReclusterFailed, err)
	}
	return nil
}

// ownedActivity checks the activity is the caller's and live.
func (uc *SourceLinks) ownedActivity(ctx context.Context, userID domain.UserID, id domain.ActivityID) error {
	act, err := uc.activities.GetActivity(ctx, id)
	if err != nil {
		return err
	}
	if act.UserID != userID || act.IsDeleted() {
		return domain.ErrNotFound
	}
	return nil
}

// ownedSource loads a non-detached source of the caller.
func (uc *SourceLinks) ownedSource(ctx context.Context, userID domain.UserID, id domain.SourceID) (domain.ActivitySource, error) {
	src, err := uc.activities.GetSource(ctx, id)
	if err != nil {
		return domain.ActivitySource{}, err
	}
	if src.UserID != userID || src.Status == domain.SourceStatusDetached {
		return domain.ActivitySource{}, domain.ErrNotFound
	}
	return src, nil
}

// representative returns one contributing source of the caller's activity.
func (uc *SourceLinks) representative(ctx context.Context, userID domain.UserID, id domain.ActivityID) (domain.ActivitySource, error) {
	if err := uc.ownedActivity(ctx, userID, id); err != nil {
		return domain.ActivitySource{}, err
	}
	srcs, err := uc.activities.ListSourcesForActivity(ctx, id)
	if err != nil {
		return domain.ActivitySource{}, fmt.Errorf("list sources: %w", err)
	}
	if len(srcs) == 0 {
		return domain.ActivitySource{}, domain.ErrNotFound
	}
	return srcs[0], nil
}

func (uc *SourceLinks) activityOfSource(ctx context.Context, id domain.SourceID) (domain.Activity, error) {
	src, err := uc.activities.GetSource(ctx, id)
	if err != nil {
		return domain.Activity{}, fmt.Errorf("reload source: %w", err)
	}
	act, err := uc.activities.GetActivity(ctx, src.ActivityID)
	if err != nil {
		return domain.Activity{}, fmt.Errorf("reload activity: %w", err)
	}
	return act, nil
}
