package activity

import (
	"context"
	"errors"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Editor errors; the primary adapters map them to their protocol's codes.
var (
	ErrEmptyPatch      = errors.New("activity: patch touches no field")
	ErrGearUnavailable = errors.New("activity: gear is not available on this instance")
	ErrGearNotFound    = errors.New("activity: gear not found")
)

// FieldNotEditableError rejects a merge-managed field, or an overlay field
// when the instance has no overlay store.
type FieldNotEditableError struct{ Field string }

func (e *FieldNotEditableError) Error() string {
	return fmt.Sprintf("field %q is not editable on this instance", e.Field)
}

// GearPicker resolves the caller's gear; gear.Service implements it.
type GearPicker interface {
	Get(ctx context.Context, userID domain.UserID, id domain.GearID) (domain.Gear, error)
	DefaultFor(ctx context.Context, userID domain.UserID, t domain.ActivityType) (domain.GearID, bool, error)
}

// Editor applies owner edits to an activity and re-merges it so the edit
// survives future imports (preserveUserEdits owns the user fields).
type Editor struct {
	activities port.ActivityAggregateStore
	recompute  *RecomputeActivityFromSources
	// ClassOverrides stores the post-merge overlay (nil → overlay fields rejected).
	ClassOverrides port.ClassificationOverrideRepo
	// Gear backs gear edits (nil → ErrGearUnavailable).
	Gear GearPicker
	// Federation announces public⇄non-public transitions (optional).
	Federation port.FederationPublisher
	// Events receives activity.updated for the owner's open streams (optional).
	Events port.ActivityEventSink
	// FollowUps re-merges with follow-ups (optional; nil → recompute only).
	FollowUps Remerger
}

// NewEditor wires the use case; the optional collaborators are fields.
func NewEditor(activities port.ActivityAggregateStore, recompute *RecomputeActivityFromSources) *Editor {
	return &Editor{activities: activities, recompute: recompute}
}

// Update applies patch to the caller's live activity and returns the
// re-merged row. Another user's or a deleted activity is domain.ErrNotFound.
func (uc *Editor) Update(ctx context.Context, userID domain.UserID, id domain.ActivityID, patch domain.ActivityPatch) (domain.Activity, error) {
	act, err := uc.load(ctx, userID, id, patch)
	if err != nil {
		return domain.Activity{}, err
	}

	prevPrivacy := act.Privacy
	applyUserFields(&act, patch)
	if patch.Gear != nil {
		if err := uc.applyGear(ctx, &act, *patch.Gear); err != nil {
			return domain.Activity{}, err
		}
	}
	if err := uc.saveOverlay(ctx, id, patch.Overlay); err != nil {
		return domain.Activity{}, err
	}
	if err := uc.activities.SaveActivity(ctx, act); err != nil {
		return domain.Activity{}, fmt.Errorf("save activity: %w", err)
	}
	if err := uc.remerge(ctx, id); err != nil {
		return domain.Activity{}, fmt.Errorf("recompute activity: %w", err)
	}

	updated, err := uc.activities.GetActivity(ctx, id)
	if err != nil {
		return domain.Activity{}, fmt.Errorf("reload activity: %w", err)
	}
	if patch.Privacy != nil {
		uc.federate(ctx, prevPrivacy, updated)
	}
	if uc.Events != nil {
		uc.Events.Publish(domain.NewActivityEvent(domain.ActivityEventActivityUpdated, updated.UserID, "activity.updated").For(updated.ID, domain.SourceID{}))
	}
	return updated, nil
}

// load fetches the caller's live activity and rejects patches it cannot apply.
func (uc *Editor) load(ctx context.Context, userID domain.UserID, id domain.ActivityID, patch domain.ActivityPatch) (domain.Activity, error) {
	act, err := uc.activities.GetActivity(ctx, id)
	if err != nil {
		return domain.Activity{}, err
	}
	if act.UserID != userID || act.IsDeleted() {
		return domain.Activity{}, domain.ErrNotFound
	}
	if patch.Empty() {
		return domain.Activity{}, ErrEmptyPatch
	}
	if patch.PrimaryStreamSourceID != nil {
		return domain.Activity{}, &FieldNotEditableError{Field: "primary_stream_source_id"}
	}
	if f := patch.Overlay.FirstField(); f != "" && uc.ClassOverrides == nil {
		return domain.Activity{}, &FieldNotEditableError{Field: f}
	}
	return act, nil
}

// applyUserFields copies the merge-preserved user fields of the patch.
func applyUserFields(act *domain.Activity, p domain.ActivityPatch) {
	if p.Title != nil {
		act.Title = *p.Title
	}
	if p.Description != nil {
		act.Description = *p.Description
	}
	if p.Tags != nil {
		act.Tags = *p.Tags
	}
	if p.Privacy != nil {
		act.Privacy = *p.Privacy
	}
}

// applyGear makes a user choice sticky (GearSetByUser) or hands it back to the merge.
func (uc *Editor) applyGear(ctx context.Context, act *domain.Activity, g domain.GearEdit) error {
	if uc.Gear == nil {
		return ErrGearUnavailable
	}
	if g.Reset {
		return uc.resetGear(ctx, act)
	}
	act.GearID = nil
	if g.GearID != nil {
		owned, err := uc.Gear.Get(ctx, act.UserID, *g.GearID)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrGearNotFound, err)
		}
		act.GearID = &owned.ID
	}
	act.GearSetBy = domain.GearSetByUser
	return nil
}

// resetGear lets recompute re-derive the gear; a provider-less activity
// (manual, upload) gets the type default here, since recompute applies that
// only on the first merge.
func (uc *Editor) resetGear(ctx context.Context, act *domain.Activity) error {
	act.GearID = nil
	act.GearSetBy = domain.GearSetByMerge
	sources, err := uc.activities.ListSourcesForActivity(ctx, act.ID)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}
	for _, src := range sources {
		if src.ExternalAccountID != nil {
			return nil
		}
	}
	gid, ok, err := uc.Gear.DefaultFor(ctx, act.UserID, act.Type)
	if err != nil {
		return fmt.Errorf("gear default: %w", err)
	}
	if ok {
		act.GearID = &gid
	}
	return nil
}

// saveOverlay merges the overlay edits into the stored overlay.
func (uc *Editor) saveOverlay(ctx context.Context, id domain.ActivityID, p domain.OverlayPatch) error {
	if p.Empty() {
		return nil
	}
	co, err := uc.ClassOverrides.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("load classification override: %w", err)
	}
	co.ActivityID = id
	p.ApplyTo(&co)
	if err := uc.ClassOverrides.Set(ctx, co); err != nil {
		return fmt.Errorf("save classification override: %w", err)
	}
	return nil
}

func (uc *Editor) remerge(ctx context.Context, id domain.ActivityID) error {
	if uc.FollowUps != nil {
		_, err := uc.FollowUps.Remerge(ctx, id)
		return err
	}
	_, err := uc.recompute.Execute(ctx, id)
	return err
}

// federate announces going public (the ingest-time publish only covers
// public-at-import) and retracts leaving public. Best-effort.
func (uc *Editor) federate(ctx context.Context, prev domain.ActivityPrivacy, updated domain.Activity) {
	if uc.Federation == nil || prev == updated.Privacy {
		return
	}
	switch {
	case updated.Privacy == domain.PrivacyPublic:
		uc.Federation.PublishCreate(ctx, updated)
	case prev == domain.PrivacyPublic:
		uc.Federation.PublishDelete(ctx, updated.UserID, updated.ID)
	}
}
