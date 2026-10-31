package instance

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeUserPolicies struct {
	port.UserSettingsRepo
	user, defaults map[domain.ActivityType]domain.MergePolicy
	defaultsErr    error
}

func (f *fakeUserPolicies) GetAllMergePolicies(context.Context, domain.UserID) (map[domain.ActivityType]domain.MergePolicy, error) {
	return f.user, nil
}

func (f *fakeUserPolicies) SetMergePolicies(_ context.Context, _ domain.UserID, p map[domain.ActivityType]domain.MergePolicy) error {
	f.user = p
	return nil
}

func (f *fakeUserPolicies) GetInstanceMergeDefaults(context.Context) (map[domain.ActivityType]domain.MergePolicy, error) {
	return f.defaults, f.defaultsErr
}

func TestMergePolicies(t *testing.T) {
	repo := &fakeUserPolicies{defaults: map[domain.ActivityType]domain.MergePolicy{domain.ActivityTypeRun: {DefaultPriority: []string{"garmin"}}}}
	m := &MergePolicies{Settings: repo}
	ctx, uid := context.Background(), domain.UserID(uuid.New())
	want := map[domain.ActivityType]domain.MergePolicy{domain.ActivityTypeRide: {DefaultPriority: []string{"strava"}}}
	if err := m.Set(ctx, uid, want); err != nil {
		t.Fatal(err)
	}
	user, defaults, err := m.WithDefaults(ctx, uid)
	if err != nil || len(user) != 1 || len(defaults) != 1 || user[domain.ActivityTypeRide].DefaultPriority[0] != "strava" {
		t.Fatalf("got %v %v %v", user, defaults, err)
	}
	repo.defaultsErr = errors.New("db down")
	if _, _, err := m.WithDefaults(ctx, uid); err == nil {
		t.Error("defaults failure swallowed")
	}
	if got, err := m.ForUser(ctx, uid); err != nil || len(got) != 1 {
		t.Errorf("ForUser: %v %v", got, err)
	}
}
