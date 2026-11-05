package fleet

import (
	"context"
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

type fakeRegistry struct {
	port.ProviderRegistry
	metas []domain.ProviderMeta
}

func (f *fakeRegistry) List(context.Context) ([]domain.ProviderMeta, error) { return f.metas, nil }
func (f *fakeRegistry) Delete(_ context.Context, id string) error {
	for i, m := range f.metas {
		if m.ID == id {
			f.metas = append(f.metas[:i:i], f.metas[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound
}

func TestFleetProviders(t *testing.T) {
	ctx := context.Background()
	none := &Fleet{}
	if _, err := none.Providers(ctx); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("without a registry: %v", err)
	}
	if ids := none.ProviderIDs(ctx); len(ids) != 0 {
		t.Fatalf("ids without a registry: %v", ids)
	}
	if err := none.UnbindProvider(ctx, audit.Actor{}, "strava"); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("unbind without a registry: %v", err)
	}

	reg := &fakeRegistry{metas: []domain.ProviderMeta{{ID: "garmin", Package: "garmin"}, {ID: "strava", Package: "strava"}}}
	f := &Fleet{Registry: reg}
	if ids := f.ProviderIDs(ctx); len(ids) != 2 || ids[0] != "garmin" || ids[1] != "strava" {
		t.Fatalf("ids %v", ids)
	}
	if err := f.UnbindProvider(ctx, audit.Actor{}, "Not Valid"); kindOf(t, err) != port.ActionErrorInvalid {
		t.Fatalf("invalid id: %v", err)
	}
	if err := f.UnbindProvider(ctx, audit.Actor{}, "polar"); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("unknown provider: %v", err)
	}
	if err := f.UnbindProvider(ctx, audit.Actor{}, "strava"); err != nil {
		t.Fatal(err)
	}
	if ids := f.ProviderIDs(ctx); len(ids) != 1 || ids[0] != "garmin" {
		t.Fatalf("after unbind %v", ids)
	}
}
