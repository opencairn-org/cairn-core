package webhooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

type fakeRepo struct {
	rotatedAt map[domain.ExternalAccountID]*time.Time
	provider  map[domain.ExternalAccountID]string
	lastSel   struct {
		provider string
		before   *time.Time
		ids      []domain.ExternalAccountID
	}
}

func (f *fakeRepo) ListWebhookTokens(_ context.Context, provider string, _, _ int) ([]domain.WebhookSigningToken, int, error) {
	var out []domain.WebhookSigningToken
	for id, p := range f.provider {
		if provider == "" || p == provider {
			out = append(out, domain.WebhookSigningToken{ExternalAccountID: id, Provider: p, RotatedAt: f.rotatedAt[id]})
		}
	}
	return out, len(out), nil
}

func (f *fakeRepo) SelectAccountsForRotation(_ context.Context, provider string, before *time.Time, ids []domain.ExternalAccountID) ([]domain.ExternalAccountID, error) {
	f.lastSel.provider, f.lastSel.before, f.lastSel.ids = provider, before, ids
	var out []domain.ExternalAccountID
	for id, p := range f.provider {
		if provider != "" && p != provider {
			continue
		}
		if before != nil && f.rotatedAt[id] != nil && !f.rotatedAt[id].Before(*before) {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

func (f *fakeRepo) SetWebhookSigningToken(_ context.Context, id domain.ExternalAccountID, hash []byte, at time.Time) error {
	if len(hash) != 32 {
		return errors.New("hash must be sha256")
	}
	f.rotatedAt[id] = &at
	return nil
}

func TestBulkRotate(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(1_000_000, 0).UTC()
	fresh := now.Add(-time.Hour)
	stravaOld, stravaFresh, garmin := domain.ExternalAccountID(uuid.New()), domain.ExternalAccountID(uuid.New()), domain.ExternalAccountID(uuid.New())
	repo := &fakeRepo{
		provider:  map[domain.ExternalAccountID]string{stravaOld: "strava", stravaFresh: "strava", garmin: "garmin"},
		rotatedAt: map[domain.ExternalAccountID]*time.Time{stravaFresh: &fresh},
	}
	tok := &Tokens{Repo: repo, Now: func() time.Time { return now }}

	_, err := tok.BulkRotate(ctx, audit.Actor{}, domain.WebhookRotationSelector{})
	var ae *port.ActionError
	if !errors.As(err, &ae) || ae.Kind != port.ActionErrorInvalid {
		t.Fatalf("empty selector: %v", err)
	}
	day := 24 * time.Hour
	rotated, err := tok.BulkRotate(ctx, audit.Actor{}, domain.WebhookRotationSelector{Provider: "strava", OlderThan: &day})
	if err != nil || len(rotated) != 1 || rotated[0] != stravaOld {
		t.Fatalf("rotate stale strava: %v %v", err, rotated)
	}
	if repo.lastSel.before == nil || !repo.lastSel.before.Equal(now.Add(-day)) {
		t.Fatalf("cut-off: %v", repo.lastSel.before)
	}
	if got := repo.rotatedAt[stravaOld]; got == nil || !got.Equal(now) {
		t.Fatalf("rotated_at: %v", got)
	}
	rows, total, err := tok.List(ctx, "garmin", 10, 0)
	if err != nil || total != 1 || rows[0].RotatedAt != nil {
		t.Fatalf("list: %v %d %+v", err, total, rows)
	}
	if _, _, err := (&Tokens{}).List(ctx, "", 0, 0); !errors.As(err, &ae) || ae.Kind != port.ActionErrorUnavailable {
		t.Fatalf("no repo: %v", err)
	}
	if err := tok.Rotate(ctx, audit.Actor{}, garmin); err != nil || repo.rotatedAt[garmin] == nil || !repo.rotatedAt[garmin].Equal(now) {
		t.Fatalf("rotate one: %v %v", err, repo.rotatedAt[garmin])
	}
	if err := (&Tokens{}).Rotate(ctx, audit.Actor{}, garmin); !errors.As(err, &ae) || ae.Kind != port.ActionErrorUnavailable {
		t.Fatalf("rotate without repo: %v", err)
	}
}
