package owndata

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeActivities struct {
	port.ActivityRepo
	act       domain.Activity
	lastLimit int
}

func (f *fakeActivities) ListRecentActivitiesForUser(_ context.Context, _ domain.UserID, limit int) ([]domain.Activity, error) {
	f.lastLimit = limit
	return nil, nil
}
func (f *fakeActivities) GetActivity(context.Context, domain.ActivityID) (domain.Activity, error) {
	return f.act, nil
}

func TestRecentClampsLimit(t *testing.T) {
	acts := &fakeActivities{}
	r := &Reader{Activities: acts}
	for in, want := range map[int]int{0: DefaultLimit, -3: DefaultLimit, 5: 5, 500: MaxLimit} {
		_, _ = r.Recent(context.Background(), domain.UserID(uuid.New()), in)
		if acts.lastLimit != want {
			t.Errorf("Recent(%d) asked for %d, want %d", in, acts.lastLimit, want)
		}
	}
}

func TestActivityIsOwnerOnly(t *testing.T) {
	owner := domain.UserID(uuid.New())
	r := &Reader{Activities: &fakeActivities{act: domain.Activity{UserID: owner}}}
	if _, err := r.Activity(context.Background(), owner, domain.ActivityID(uuid.New())); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if _, err := r.Activity(context.Background(), domain.UserID(uuid.New()), domain.ActivityID(uuid.New())); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stranger: %v", err)
	}
}
