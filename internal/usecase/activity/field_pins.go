package activity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ErrPinSourceNotOnActivity: the pinned source belongs to another activity.
var ErrPinSourceNotOnActivity = errors.New("activity: pinned source not on this activity")

// FieldPins manage per-field manual source pins on an activity the caller
// already owns (see SourceActions.OwnedActivity). A change re-merges at once;
// that re-merge is best-effort, the pin itself is durable.
type FieldPins struct {
	Activities port.ActivityRepo
	Overrides  port.FieldOverrideRepo
	Remerge    Remerger
	Log        *slog.Logger
}

// List returns the activity's pins.
func (p *FieldPins) List(ctx context.Context, act domain.Activity) ([]domain.FieldSourceOverride, error) {
	return p.Overrides.ListForActivity(ctx, act.ID)
}

// Set pins field group g of act to one of its sources.
func (p *FieldPins) Set(ctx context.Context, act domain.Activity, g domain.FieldGroup, sourceID domain.SourceID) error {
	src, err := p.Activities.GetSource(ctx, sourceID)
	if err != nil || src.ActivityID != act.ID {
		return ErrPinSourceNotOnActivity
	}
	if err := p.Overrides.Set(ctx, domain.FieldSourceOverride{ActivityID: act.ID, FieldKey: g, SourceID: sourceID}); err != nil {
		return fmt.Errorf("set field pin: %w", err)
	}
	p.remerge(ctx, act.ID, "recompute after field pin failed")
	return nil
}

// Clear removes the pin of field group g; absent pins are a no-op.
func (p *FieldPins) Clear(ctx context.Context, act domain.Activity, g domain.FieldGroup) error {
	if err := p.Overrides.Delete(ctx, act.ID, g); err != nil {
		return fmt.Errorf("clear field pin: %w", err)
	}
	p.remerge(ctx, act.ID, "recompute after field unpin failed")
	return nil
}

func (p *FieldPins) remerge(ctx context.Context, id domain.ActivityID, msg string) {
	if p.Remerge == nil {
		return
	}
	if _, err := p.Remerge.Remerge(ctx, id); err != nil {
		log := p.Log
		if log == nil {
			log = slog.Default()
		}
		log.Warn(msg, "activity_id", id, "error", err)
	}
}
