// Package athlete manages the athlete physiology profile: dated FTP, weight,
// threshold HR, … values that feed the TSS estimate as of an activity's date.
package athlete

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	"github.com/opencairn-org/cairn-core/internal/usecase/trainingload"
)

// ErrRecomputeUnavailable: Recalculate has no recompute wired.
var ErrRecomputeUnavailable = errors.New("athlete: recompute unavailable")

// recalcWarmUpDays seeds CTL/ATL before the earliest re-derived activity.
const recalcWarmUpDays = 42

// Profile is the user's athlete profile plus its retroactive recalculation.
type Profile struct {
	Repo         port.AthleteProfileRepo
	Activities   port.ActivityRepo
	Recompute    *activity.RecomputeActivityFromSources   // nil → Recalculate unavailable
	TrainingLoad *trainingload.ComputeTrainingLoadForUser // nil → curves left as they are
	Log          *slog.Logger
	Now          func() time.Time
}

func (p *Profile) now() time.Time {
	if p.Now == nil {
		return time.Now().UTC()
	}
	return p.Now()
}

func (p *Profile) log() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

// Entries returns all of the user's dated values.
func (p *Profile) Entries(ctx context.Context, userID domain.UserID) ([]domain.AthleteMetricEntry, error) {
	return p.Repo.ListEntries(ctx, userID)
}

// Put validates and upserts one dated value (keyed by user, key and date).
func (p *Profile) Put(ctx context.Context, e domain.AthleteMetricEntry) (domain.AthleteMetricID, error) {
	if err := e.Validate(); err != nil {
		return domain.AthleteMetricID{}, port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	id, err := p.Repo.UpsertEntry(ctx, e)
	if err != nil {
		return domain.AthleteMetricID{}, fmt.Errorf("upsert athlete metric: %w", err)
	}
	return id, nil
}

// Delete removes one of the user's entries.
func (p *Profile) Delete(ctx context.Context, userID domain.UserID, id domain.AthleteMetricID) error {
	return p.Repo.DeleteEntry(ctx, userID, id)
}

// Recalculation reports what Recalculate re-derived.
type Recalculation struct {
	Recomputed       int
	TrainingLoadDays int
}

// Recalculate re-derives TSS for every activity of the user under the current
// profile, then the training-load curves from the earliest one, so a profile
// edit corrects the CTL/ATL/TSB history. Per-activity failures are skipped.
func (p *Profile) Recalculate(ctx context.Context, userID domain.UserID) (Recalculation, error) {
	if p.Recompute == nil {
		return Recalculation{}, ErrRecomputeUnavailable
	}
	// A wide window covers the full history.
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	end := p.now().Add(24 * time.Hour)
	acts, err := p.Activities.ListActivitiesForUser(ctx, userID, start, end)
	if err != nil {
		return Recalculation{}, fmt.Errorf("list activities: %w", err)
	}
	var out Recalculation
	earliest := end
	for _, a := range acts {
		if _, err := p.Recompute.Execute(ctx, a.ID); err != nil {
			p.log().Warn("athlete recalc: recompute failed", "activity", a.ID, "error", err)
			continue
		}
		if a.StartTime.Before(earliest) {
			earliest = a.StartTime
		}
		out.Recomputed++
	}
	if p.TrainingLoad == nil || out.Recomputed == 0 {
		return out, nil
	}
	res, err := p.TrainingLoad.Execute(ctx, trainingload.Input{UserID: userID, Start: earliest, End: p.now(), WarmUpDays: recalcWarmUpDays})
	if err != nil {
		p.log().Warn("athlete recalc: training load failed", "error", err)
		return out, nil
	}
	out.TrainingLoadDays = res.DaysComputed
	return out, nil
}
