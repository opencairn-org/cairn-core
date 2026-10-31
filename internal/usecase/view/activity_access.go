package view

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ActivityAccess loads one activity for a caller: the owner's own, one a
// viewer may see a category of, or any live one (signed-URL reads, where the
// token is the authorization). Every refusal is domain.ErrNotFound.
type ActivityAccess struct {
	activities port.ActivityReader
	visibility *Visibility
}

// NewActivityAccess wires the loader.
func NewActivityAccess(activities port.ActivityReader, visibility *Visibility) *ActivityAccess {
	return &ActivityAccess{activities: activities, visibility: visibility}
}

// Owned is me's live activity.
func (a *ActivityAccess) Owned(ctx context.Context, me domain.UserID, id domain.ActivityID) (domain.Activity, error) {
	act, err := a.activities.GetActivity(ctx, id)
	if err != nil || act.UserID != me || act.IsDeleted() {
		return domain.Activity{}, domain.ErrNotFound
	}
	return act, nil
}

// Live is any activity that is not deleted.
func (a *ActivityAccess) Live(ctx context.Context, id domain.ActivityID) (domain.Activity, error) {
	act, err := a.activities.GetActivity(ctx, id)
	if err != nil || act.IsDeleted() {
		return domain.Activity{}, domain.ErrNotFound
	}
	return act, nil
}

// Visible is the activity when viewer (nil = anonymous) may see cat of it;
// owners always may.
func (a *ActivityAccess) Visible(ctx context.Context, id domain.ActivityID, viewer *domain.UserID, hasLink bool, cat domain.Category) (domain.Activity, Viewer, error) {
	act, err := a.activities.GetActivity(ctx, id)
	if err != nil {
		return domain.Activity{}, Viewer{}, domain.ErrNotFound
	}
	vw, ok := a.visibility.Resolve(ctx, act, viewer, hasLink)
	if !ok || (!vw.IsOwner && !vw.Cats.Has(cat)) {
		return domain.Activity{}, Viewer{}, domain.ErrNotFound
	}
	return act, vw, nil
}
