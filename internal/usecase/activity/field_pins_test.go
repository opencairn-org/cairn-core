package activity

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// memOverrides is a map-backed FieldOverrideRepo.
type memOverrides struct {
	port.FieldOverrideRepo
	pins map[domain.FieldGroup]domain.FieldSourceOverride
}

func (m *memOverrides) ListForActivity(context.Context, domain.ActivityID) ([]domain.FieldSourceOverride, error) {
	out := make([]domain.FieldSourceOverride, 0, len(m.pins))
	for _, p := range m.pins {
		out = append(out, p)
	}
	return out, nil
}

func (m *memOverrides) Set(_ context.Context, o domain.FieldSourceOverride) error {
	m.pins[o.FieldKey] = o
	return nil
}

func (m *memOverrides) Delete(_ context.Context, _ domain.ActivityID, g domain.FieldGroup) error {
	delete(m.pins, g)
	return nil
}

// remergeSpy counts re-merges and can fail them.
type remergeSpy struct {
	calls int
	err   error
}

func (r *remergeSpy) Remerge(context.Context, domain.ActivityID) (RecomputeResult, error) {
	r.calls++
	return RecomputeResult{}, r.err
}

func TestFieldPins_SetListClear(t *testing.T) {
	f := newSAFixture(t, 2)
	ov := &memOverrides{pins: map[domain.FieldGroup]domain.FieldSourceOverride{}}
	spy := &remergeSpy{err: errors.New("remerge down")}
	pins := &FieldPins{Activities: f.repo, Overrides: ov, Remerge: spy}
	ctx := context.Background()
	act := f.repo.Activities[f.act]

	// A failed re-merge does not fail the durable pin.
	if err := pins.Set(ctx, act, domain.FieldGroupTitle, f.srcs[1]); err != nil {
		t.Fatal(err)
	}
	got, _ := pins.List(ctx, act)
	if len(got) != 1 || got[0].SourceID != f.srcs[1] {
		t.Fatalf("pins %v", got)
	}
	if err := pins.Clear(ctx, act, domain.FieldGroupTitle); err != nil {
		t.Fatal(err)
	}
	if got, _ := pins.List(ctx, act); len(got) != 0 || spy.calls != 2 {
		t.Errorf("after clear: pins %v, remerges %d", got, spy.calls)
	}
}

func TestFieldPins_RefusesForeignSource(t *testing.T) {
	f := newSAFixture(t, 1)
	other := newSAFixture(t, 1)
	for id, s := range other.repo.Sources {
		f.repo.Sources[id] = s
	}
	ov := &memOverrides{pins: map[domain.FieldGroup]domain.FieldSourceOverride{}}
	pins := &FieldPins{Activities: f.repo, Overrides: ov}
	act := f.repo.Activities[f.act]
	for _, sid := range []domain.SourceID{other.srcs[0], domain.SourceID(uuid.New())} {
		if err := pins.Set(context.Background(), act, domain.FieldGroupTitle, sid); !errors.Is(err, ErrPinSourceNotOnActivity) {
			t.Errorf("%s: %v", sid, err)
		}
	}
	if len(ov.pins) != 0 {
		t.Error("foreign pin stored")
	}
}

func TestSourceActions_SourceOf(t *testing.T) {
	f := newSAFixture(t, 1)
	ctx := context.Background()
	if src, err := f.uc.SourceOf(ctx, f.user, f.act, f.srcs[0], false); err != nil || src.ID != f.srcs[0] {
		t.Fatalf("own source: %v %v", src.ID, err)
	}
	if _, err := f.uc.SourceOf(ctx, domain.UserID(uuid.New()), f.act, f.srcs[0], true); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("stranger: %v", err)
	}
	other := newSAFixture(t, 1)
	f.repo.Sources[other.srcs[0]] = other.repo.Sources[other.srcs[0]]
	if _, err := f.uc.SourceOf(ctx, f.user, f.act, other.srcs[0], true); !errors.Is(err, ErrSourceNotOnActivity) {
		t.Errorf("other activity's source: %v", err)
	}

	a := f.repo.Activities[f.act]
	a.DeletedAt = &saNow
	f.repo.Activities[f.act] = a
	if _, err := f.uc.SourceOf(ctx, f.user, f.act, f.srcs[0], false); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleted, live only: %v", err)
	}
	if _, err := f.uc.SourceOf(ctx, f.user, f.act, f.srcs[0], true); err != nil {
		t.Errorf("deleted, withDeleted: %v", err)
	}
}
