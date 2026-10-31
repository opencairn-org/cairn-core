package athlete

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeProfiles struct {
	port.AthleteProfileRepo
	saved []domain.AthleteMetricEntry
}

func (f *fakeProfiles) UpsertEntry(_ context.Context, e domain.AthleteMetricEntry) (domain.AthleteMetricID, error) {
	f.saved = append(f.saved, e)
	return domain.AthleteMetricID(uuid.New()), nil
}

func TestPutValidates(t *testing.T) {
	repo := &fakeProfiles{}
	p := &Profile{Repo: repo}
	uid := domain.UserID(uuid.New())
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, err := p.Put(context.Background(), domain.AthleteMetricEntry{UserID: uid, Key: "bogus", EffectiveDate: day, Value: 1})
	var ae *port.ActionError
	if !errors.As(err, &ae) || ae.Kind != port.ActionErrorInvalid {
		t.Errorf("unknown key: %v", err)
	}
	if _, err := p.Put(context.Background(), domain.AthleteMetricEntry{UserID: uid, Key: domain.AthleteFTPWatts, EffectiveDate: day, Value: 250}); err != nil {
		t.Fatal(err)
	}
	if len(repo.saved) != 1 {
		t.Errorf("saved %d", len(repo.saved))
	}
}

func TestRecalculateNeedsRecompute(t *testing.T) {
	if _, err := (&Profile{}).Recalculate(context.Background(), domain.UserID(uuid.New())); !errors.Is(err, ErrRecomputeUnavailable) {
		t.Errorf("got %v", err)
	}
}
