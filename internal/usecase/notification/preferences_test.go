package notification

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type prefStore struct {
	rows []domain.NotificationPreference
}

func (s *prefStore) ListForUser(context.Context, domain.UserID) ([]domain.NotificationPreference, error) {
	return s.rows, nil
}
func (s *prefStore) Upsert(_ context.Context, p domain.NotificationPreference) error {
	for i, r := range s.rows {
		if r.Type == p.Type && r.Channel == p.Channel {
			s.rows[i] = p
			return nil
		}
	}
	s.rows = append(s.rows, p)
	return nil
}

func TestPreferencesMatrixAndSet(t *testing.T) {
	ctx := t.Context()
	me := domain.UserID(uuid.New())
	store := &prefStore{}
	p := &Preferences{Repo: store}

	cells, err := p.Matrix(ctx, me, domain.NotificationChannelEmail)
	if err != nil || len(cells) != len(domain.UserFacingNotificationTypes) {
		t.Fatalf("matrix: %v %d", err, len(cells))
	}
	for _, c := range cells {
		if c.Explicit || c.Enabled != domain.DefaultChannelEnabled(c.Type, domain.NotificationChannelEmail) {
			t.Fatalf("default cell: %+v", c)
		}
	}
	first := domain.UserFacingNotificationTypes[0]
	want := !domain.DefaultChannelEnabled(first, domain.NotificationChannelEmail)
	if err := p.Set(ctx, me, first, domain.NotificationChannelEmail, want); err != nil {
		t.Fatal(err)
	}
	cells, _ = p.Matrix(ctx, me, domain.NotificationChannelEmail)
	if !cells[0].Explicit || cells[0].Enabled != want || cells[1].Explicit {
		t.Fatalf("after set: %+v", cells[:2])
	}
	var ae *port.ActionError
	if err := p.Set(ctx, me, first, domain.NotificationChannelWebhook, true); !errors.As(err, &ae) || ae.Kind != port.ActionErrorInvalid {
		t.Fatalf("channel: %v", err)
	}
	if err := p.Set(ctx, me, domain.NotificationType(99), domain.NotificationChannelEmail, true); !errors.As(err, &ae) || ae.Kind != port.ActionErrorInvalid {
		t.Fatalf("type: %v", err)
	}
	if _, err := (&Preferences{}).Matrix(ctx, me, domain.NotificationChannelEmail); !errors.As(err, &ae) || ae.Kind != port.ActionErrorUnavailable {
		t.Fatalf("unwired: %v", err)
	}
}
