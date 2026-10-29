package besteffort

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeCustoms struct {
	port.CustomBestEffortRepo
	rows []domain.CustomBestEffort
}

func (f *fakeCustoms) Create(_ context.Context, c domain.CustomBestEffort) (domain.CustomBestEffortID, error) {
	for _, r := range f.rows {
		if r.ActivityType == c.ActivityType && r.Metric == c.Metric && r.WindowKind == c.WindowKind && r.WindowValue == c.WindowValue {
			return domain.CustomBestEffortID{}, domain.ErrCustomEffortDuplicate
		}
	}
	c.ID = domain.CustomBestEffortID(uuid.New())
	f.rows = append(f.rows, c)
	return c.ID, nil
}
func (f *fakeCustoms) ListForUser(_ context.Context, _ domain.UserID, at domain.ActivityType) ([]domain.CustomBestEffort, error) {
	var out []domain.CustomBestEffort
	for _, r := range f.rows {
		if at == "" || r.ActivityType == at {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeCustoms) Delete(_ context.Context, _ domain.UserID, id domain.CustomBestEffortID) error {
	for i, r := range f.rows {
		if r.ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound
}

type fakeEffortWindows struct {
	port.BestEffortRepo
	deleted []int
}

func (f *fakeEffortWindows) DeleteForUserWindow(_ context.Context, _ domain.UserID, _ domain.ActivityType, _ domain.BestEffortMetric, _ domain.BestEffortWindowKind, v int) error {
	f.deleted = append(f.deleted, v)
	return nil
}

func kind(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func TestCustomsCreateDelete(t *testing.T) {
	repo := &fakeCustoms{}
	efforts := &fakeEffortWindows{}
	c := &Customs{Repo: repo, BestEfforts: efforts}
	uid := domain.UserID(uuid.New())
	in := domain.CustomBestEffort{UserID: uid, ActivityType: domain.ActivityTypeRun, Metric: domain.BestEffortMetricPace, WindowKind: domain.BestEffortWindowDistance, WindowValue: 7500, Label: "loop"}

	if _, err := c.Create(t.Context(), domain.CustomBestEffort{UserID: uid}); kind(t, err) != port.ActionErrorInvalid {
		t.Fatalf("invalid: %v", err)
	}
	created, err := c.Create(t.Context(), in)
	if err != nil || created.ID == (domain.CustomBestEffortID{}) {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.Create(t.Context(), in); kind(t, err) != port.ActionErrorConflict {
		t.Fatalf("duplicate: %v", err)
	}
	// A custom window equal to a standard one never deletes standard efforts.
	std, err := c.Create(t.Context(), domain.CustomBestEffort{UserID: uid, ActivityType: domain.ActivityTypeRun, Metric: domain.BestEffortMetricPace, WindowKind: domain.BestEffortWindowDistance, WindowValue: 5000, Label: "5k"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), uid, std.ID); err != nil || len(efforts.deleted) != 0 {
		t.Fatalf("standard delete: %v %v", err, efforts.deleted)
	}
	if err := c.Delete(t.Context(), uid, created.ID); err != nil || len(efforts.deleted) != 1 || efforts.deleted[0] != 7500 {
		t.Fatalf("custom delete: %v %v", err, efforts.deleted)
	}
	if err := c.Delete(t.Context(), uid, created.ID); kind(t, err) != port.ActionErrorNotFound {
		t.Fatalf("gone: %v", err)
	}
	for i := 0; i < domain.MaxCustomBestEfforts; i++ {
		repo.rows = append(repo.rows, domain.CustomBestEffort{ID: domain.CustomBestEffortID(uuid.New()), UserID: uid, WindowValue: i})
	}
	if _, err := c.Create(t.Context(), in); kind(t, err) != port.ActionErrorQuota {
		t.Fatalf("limit: %v", err)
	}
}

func TestMergeWindows(t *testing.T) {
	customs := []domain.CustomBestEffort{
		{Metric: domain.BestEffortMetricPace, WindowKind: domain.BestEffortWindowDistance, WindowValue: 5000}, // standard already
		{Metric: domain.BestEffortMetricPace, WindowKind: domain.BestEffortWindowDistance, WindowValue: 7500},
	}
	got := mergeWindows(StandardWindows, customs)
	if len(got) != len(StandardWindows)+1 || got[len(got)-1].Value != 7500 {
		t.Fatalf("merged %d windows, last %+v", len(got), got[len(got)-1])
	}
}
