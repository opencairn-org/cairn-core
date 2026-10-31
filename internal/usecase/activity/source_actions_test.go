package activity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

var saNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// fedSpy records PublishDelete calls.
type fedSpy struct{ deleted []domain.ActivityID }

func (f *fedSpy) PublishCreate(context.Context, domain.Activity) {}
func (f *fedSpy) PublishDelete(_ context.Context, _ domain.UserID, id domain.ActivityID) {
	f.deleted = append(f.deleted, id)
}

type saFixture struct {
	uc   *SourceActions
	repo *inmem.ActivityRepo
	deny *inmem.SourceDenylist
	fed  *fedSpy
	user domain.UserID
	act  domain.ActivityID
	srcs []domain.SourceID
}

// newSAFixture seeds one activity of the user with n active sources.
func newSAFixture(t *testing.T, n int) *saFixture {
	t.Helper()
	f := &saFixture{repo: inmem.NewActivityRepo(), deny: &inmem.SourceDenylist{}, fed: &fedSpy{}, user: domain.UserID(uuid.New()), act: domain.ActivityID(uuid.New())}
	f.repo.Activities[f.act] = domain.Activity{ID: f.act, UserID: f.user, Type: domain.ActivityTypeRide, StartTime: saNow, Privacy: domain.PrivacyPrivate}
	for i := range n {
		sid := domain.SourceID(uuid.New())
		d := 10000.0
		f.repo.Sources[sid] = domain.ActivitySource{
			ID: sid, ActivityID: f.act, UserID: f.user, Provider: "garmin", ExternalID: "ext-" + sid.String(),
			Status: domain.SourceStatusActive, ImportedAt: saNow,
			Parsed: domain.ActivitySourcePayload{
				Type: domain.ActivityTypeRide, StartTime: saNow.Add(time.Duration(i) * time.Second), EndTime: saNow.Add(time.Hour),
				ElapsedDuration: time.Hour, MovingDuration: 50 * time.Minute, Timezone: "UTC",
				Summary: domain.ActivitySummary{DistanceM: &d},
			},
		}
		f.srcs = append(f.srcs, sid)
	}
	recompute := NewRecomputeActivityFromSources(f.repo, inmem.DefaultSettings{}, nil, nil, nil, nil, inmem.PassTx{}, func() time.Time { return saNow })
	f.uc = NewSourceActions(f.repo, f.deny, recompute, f.fed, func() time.Time { return saNow })
	return f
}

func TestSourceActions_DetachKeepsActivityWhenSourcesRemain(t *testing.T) {
	f := newSAFixture(t, 2)
	res, err := f.uc.Detach(t.Context(), f.user, f.act, f.srcs[0], "")
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if !res.Recomputed || res.SoftDeleted || res.SourceCount != 1 {
		t.Fatalf("result %+v", res)
	}
	if s := f.repo.Sources[f.srcs[0]]; s.Status != domain.SourceStatusDetached || s.StatusReason != DetachReasonUser {
		t.Fatalf("source not detached: %+v", s)
	}
	if len(f.deny.Entries) != 1 || f.deny.Entries[0].ExternalID != f.repo.Sources[f.srcs[0]].ExternalID {
		t.Fatalf("denylist %+v", f.deny.Entries)
	}
	if f.repo.Activities[f.act].DeletedAt != nil || len(f.fed.deleted) != 0 {
		t.Fatal("activity must survive with one source left")
	}
}

func TestSourceActions_DetachLastSourceSoftDeletesAndFederates(t *testing.T) {
	f := newSAFixture(t, 1)
	res, err := f.uc.Detach(t.Context(), f.user, f.act, f.srcs[0], "wrong_match")
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if !res.SoftDeleted || f.repo.Activities[f.act].DeletedAt == nil {
		t.Fatalf("expected soft delete, got %+v", res)
	}
	if len(f.fed.deleted) != 1 || f.fed.deleted[0] != f.act {
		t.Fatalf("federation delete not published: %v", f.fed.deleted)
	}
	if f.deny.Entries[0].Reason != "wrong_match" {
		t.Fatalf("reason not propagated: %+v", f.deny.Entries[0])
	}
}

func TestSourceActions_DetachRefusesForeignAndMismatched(t *testing.T) {
	f := newSAFixture(t, 1)
	other := domain.UserID(uuid.New())
	if _, err := f.uc.Detach(t.Context(), other, f.act, f.srcs[0], ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign user: %v", err)
	}
	// A second activity of the same user: its source is not on f.act.
	otherAct := domain.ActivityID(uuid.New())
	f.repo.Activities[otherAct] = domain.Activity{ID: otherAct, UserID: f.user}
	foreignSrc := domain.SourceID(uuid.New())
	f.repo.Sources[foreignSrc] = domain.ActivitySource{ID: foreignSrc, ActivityID: otherAct, UserID: f.user, Status: domain.SourceStatusActive}
	if _, err := f.uc.Detach(t.Context(), f.user, f.act, foreignSrc, ""); !errors.Is(err, ErrSourceNotOnActivity) {
		t.Fatalf("mismatched pair: %v", err)
	}
	if f.repo.Sources[f.srcs[0]].Status != domain.SourceStatusActive {
		t.Fatal("nothing may be detached on a refused call")
	}
}

func TestSourceActions_DeleteDetachesAllAndSoftDeletes(t *testing.T) {
	f := newSAFixture(t, 3)
	if err := f.uc.Delete(t.Context(), f.user, f.act); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, sid := range f.srcs {
		if s := f.repo.Sources[sid]; s.Status != domain.SourceStatusDetached || s.StatusReason != DetachReasonDelete {
			t.Fatalf("source %s not detached: %+v", sid, s)
		}
	}
	if f.repo.Activities[f.act].DeletedAt == nil {
		t.Fatal("activity not soft-deleted")
	}
	if len(f.deny.Entries) != 3 || len(f.fed.deleted) != 1 {
		t.Fatalf("denylist=%d federated=%d", len(f.deny.Entries), len(f.fed.deleted))
	}
	// Deleting again is NotFound, not a second retraction.
	if err := f.uc.Delete(t.Context(), f.user, f.act); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestSourceActions_DeleteForeignIsNotFound(t *testing.T) {
	f := newSAFixture(t, 1)
	if err := f.uc.Delete(t.Context(), domain.UserID(uuid.New()), f.act); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if f.repo.Activities[f.act].DeletedAt != nil {
		t.Fatal("foreign delete must not touch the row")
	}
}

// failingDenylist refuses every entry.
type failingDenylist struct{ port.SourceDenylistRepo }

func (failingDenylist) Add(context.Context, domain.SourceDenylistEntry) error {
	return errors.New("db down")
}

func TestSourceActions_DetachFailsWhenDenylistFails(t *testing.T) {
	f := newSAFixture(t, 2)
	recompute := NewRecomputeActivityFromSources(f.repo, inmem.DefaultSettings{}, nil, nil, nil, nil, inmem.PassTx{}, func() time.Time { return saNow })
	uc := NewSourceActions(f.repo, failingDenylist{}, recompute, f.fed, func() time.Time { return saNow })
	if _, err := uc.Detach(context.Background(), f.user, f.act, f.srcs[0], ""); err == nil {
		t.Fatal("a detach whose denylist entry failed must fail, or the next sync resurrects it")
	}
	if err := uc.Delete(context.Background(), f.user, f.act); err == nil {
		t.Fatal("a delete whose denylist entries failed must fail")
	}
}
