package activity

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// reviewActivityStore reads activities and serves the review queue.
type reviewActivityStore interface {
	port.ActivityReader
	port.ActivityReviewQueue
}

// ReviewQueue is the confidence-band review queue: activities the matcher
// merged with only medium confidence, for the owner to confirm.
type ReviewQueue struct {
	Activities reviewActivityStore
}

// List returns the caller's activities awaiting review.
func (q *ReviewQueue) List(ctx context.Context, userID domain.UserID) ([]domain.ActivityReviewItem, error) {
	items, err := q.Activities.ListActivitiesNeedingReview(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("review queue: %w", err)
	}
	return items, nil
}

// MarkReviewed clears the flag on one of the caller's live activities; any
// lookup failure is domain.ErrNotFound so foreign ids stay indistinguishable.
func (q *ReviewQueue) MarkReviewed(ctx context.Context, userID domain.UserID, id domain.ActivityID) error {
	act, err := q.Activities.GetActivity(ctx, id)
	if err != nil || act.UserID != userID || act.IsDeleted() {
		return domain.ErrNotFound
	}
	if err := q.Activities.ClearNeedsReview(ctx, act.ID); err != nil {
		return fmt.Errorf("clear needs review: %w", err)
	}
	return nil
}
