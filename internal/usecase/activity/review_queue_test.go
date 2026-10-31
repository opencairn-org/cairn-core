package activity

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

type reviewRepo struct {
	*inmem.ActivityRepo
	cleared []domain.ActivityID
}

func (r *reviewRepo) ListActivitiesNeedingReview(_ context.Context, userID domain.UserID) ([]domain.ActivityReviewItem, error) {
	var out []domain.ActivityReviewItem
	for _, a := range r.Activities {
		if a.UserID == userID {
			out = append(out, domain.ActivityReviewItem{ID: a.ID, Title: a.Title, MatchConfidence: "medium"})
		}
	}
	return out, nil
}

func (r *reviewRepo) ClearNeedsReview(_ context.Context, id domain.ActivityID) error {
	r.cleared = append(r.cleared, id)
	return nil
}

func TestReviewQueue(t *testing.T) {
	repo := &reviewRepo{ActivityRepo: inmem.NewActivityRepo()}
	owner, other := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	act := domain.ActivityID(uuid.New())
	repo.Activities[act] = domain.Activity{ID: act, UserID: owner, Title: "ride", StartTime: saNow}
	q := &ReviewQueue{Activities: repo}
	ctx := context.Background()

	items, err := q.List(ctx, owner)
	if err != nil || len(items) != 1 || items[0].ID != act {
		t.Fatalf("list %+v %v", items, err)
	}
	if err := q.MarkReviewed(ctx, other, act); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign mark: %v", err)
	}
	if err := q.MarkReviewed(ctx, owner, domain.ActivityID(uuid.New())); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing mark: %v", err)
	}
	if err := q.MarkReviewed(ctx, owner, act); err != nil || len(repo.cleared) != 1 || repo.cleared[0] != act {
		t.Fatalf("mark: %v cleared=%v", err, repo.cleared)
	}
}
