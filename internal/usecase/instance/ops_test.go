package instance

import (
	"context"
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

type fakeUserSettings struct {
	port.UserSettingsRepo
	defaults map[domain.ActivityType]domain.MergePolicy
}

func (f *fakeUserSettings) GetInstanceMergeDefaults(context.Context) (map[domain.ActivityType]domain.MergePolicy, error) {
	return f.defaults, nil
}
func (f *fakeUserSettings) SetInstanceMergeDefaults(_ context.Context, p map[domain.ActivityType]domain.MergePolicy) error {
	f.defaults = p
	return nil
}

type fakeKeys struct{ active *domain.SigningKey }

func (f *fakeKeys) Bootstrap(context.Context, *domain.UserID) (domain.SigningKey, bool, error) {
	if f.active != nil {
		return *f.active, false, nil
	}
	f.active = &domain.SigningKey{PublicKey: "A1"}
	return *f.active, true, nil
}
func (f *fakeKeys) Rotate(context.Context, *domain.UserID) (domain.SigningKey, string, error) {
	old := ""
	if f.active != nil {
		old = f.active.PublicKey
	}
	f.active = &domain.SigningKey{PublicKey: "A2"}
	return *f.active, old, nil
}

type fakeGear struct{ n int }

func (g fakeGear) RebuildAllTotals(context.Context) (int, error) { return g.n, nil }

func TestMergeDefaultsAndOps(t *testing.T) {
	ctx := t.Context()
	md := &MergeDefaults{Settings: &fakeUserSettings{defaults: map[domain.ActivityType]domain.MergePolicy{}}}
	if err := md.Set(ctx, audit.Actor{}, map[domain.ActivityType]domain.MergePolicy{"yoga": {}}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bad type: %v", err)
	}
	if err := md.Set(ctx, audit.Actor{}, map[domain.ActivityType]domain.MergePolicy{domain.ActivityTypeRide: {Overrides: map[domain.FieldGroup][]string{"nope": nil}}}); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("bad field group: %v", err)
	}
	if err := md.Set(ctx, audit.Actor{}, map[domain.ActivityType]domain.MergePolicy{domain.ActivityTypeRide: {DefaultPriority: []string{"garmin"}}}); err != nil {
		t.Fatal(err)
	}
	policies, groups, err := md.Get(ctx)
	if err != nil || len(policies) != 1 || len(groups) == 0 {
		t.Fatalf("get: %v %v %v", err, policies, groups)
	}

	var ran []int
	ops := &Ops{GeocodeBackfill: func(limit int) { ran = append(ran, limit) }, Gear: fakeGear{n: 4}, Keys: &fakeKeys{}}
	if n, err := ops.RunGeocodeBackfill(ctx, audit.Actor{}, 0); err != nil || n != 50 || len(ran) != 1 {
		t.Fatalf("geocode: %d %v", n, err)
	}
	if n, err := ops.RebuildGearTotals(ctx, audit.Actor{}); err != nil || n != 4 {
		t.Fatalf("gear: %d %v", n, err)
	}
	if key, created, err := ops.BootstrapKey(ctx, audit.Actor{}); err != nil || !created || key.PublicKey != "A1" {
		t.Fatalf("bootstrap: %v %v %+v", err, created, key)
	}
	if _, created, _ := ops.BootstrapKey(ctx, audit.Actor{}); created {
		t.Fatal("second bootstrap must be a no-op")
	}
	if key, old, err := ops.RotateKey(ctx, audit.Actor{}); err != nil || key.PublicKey != "A2" || old != "A1" {
		t.Fatalf("rotate: %v %+v %q", err, key, old)
	}
	if _, err := (&Ops{}).RunGeocodeBackfill(ctx, audit.Actor{}, 1); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("no geocoder: %v", err)
	}
}
