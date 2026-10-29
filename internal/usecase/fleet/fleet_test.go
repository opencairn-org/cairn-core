package fleet

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

type fakePools struct {
	port.WorkerPoolRepo
	pools   []domain.WorkerPool
	retired []string
}

func (f *fakePools) ListAll(context.Context) ([]domain.WorkerPool, error) { return f.pools, nil }
func (f *fakePools) Retire(_ context.Context, provider, name string) error {
	for _, p := range f.pools {
		if p.Provider == provider && p.Name == name && p.RetiredAt == nil {
			f.retired = append(f.retired, name)
			return nil
		}
	}
	return domain.ErrNotFound
}

type fakeEnrollments struct {
	port.WorkerEnrollmentRepo
	extended map[domain.WorkerEnrollmentID]time.Time
	listed   port.ListEnrollmentsFilter
}

func (f *fakeEnrollments) ListEnrollments(_ context.Context, fl port.ListEnrollmentsFilter) ([]domain.WorkerEnrollment, error) {
	f.listed = fl
	return []domain.WorkerEnrollment{{Name: "w1"}}, nil
}
func (f *fakeEnrollments) ExtendEnrollment(_ context.Context, id domain.WorkerEnrollmentID, at time.Time) error {
	if _, ok := f.extended[id]; !ok {
		return domain.ErrEnrollmentNotFound
	}
	f.extended[id] = at
	return nil
}

func kindOf(t *testing.T, err error) port.ActionErrorKind {
	t.Helper()
	var ae *port.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an ActionError: %v", err)
	}
	return ae.Kind
}

func TestFleetPoolsAndEnrollments(t *testing.T) {
	gone := time.Now()
	pools := &fakePools{pools: []domain.WorkerPool{{Provider: "strava", Name: "a"}, {Provider: "strava", Name: "old", RetiredAt: &gone}}}
	known := domain.WorkerEnrollmentID(uuid.New())
	enr := &fakeEnrollments{extended: map[domain.WorkerEnrollmentID]time.Time{known: {}}}
	now := time.Unix(1000, 0).UTC()
	f := &Fleet{Pools: pools, Enrollments: enr, Now: func() time.Time { return now },
		Live: func(context.Context) []domain.WorkerHeartbeat {
			return []domain.WorkerHeartbeat{{Provider: "strava", WorkerName: "a"}, {Provider: "strava", WorkerName: "a"}}
		}}
	ctx := t.Context()

	st, err := f.PoolStatuses(ctx)
	if err != nil || len(st) != 2 || st[0].OnlineInstances != 2 || st[1].OnlineInstances != 0 {
		t.Fatalf("pools: %v %+v", err, st)
	}
	if err := f.RetirePool(ctx, audit.Actor{}, "strava", "old"); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("retire retired: %v", err)
	}
	if err := f.RetirePool(ctx, audit.Actor{}, "strava", "a"); err != nil || len(pools.retired) != 1 {
		t.Fatalf("retire: %v", err)
	}
	if _, err := f.ListEnrollments(ctx, "strava", false, true); err != nil || enr.listed.Provider != "strava" || enr.listed.IncludeRevoked || !enr.listed.IncludeExpired {
		t.Fatalf("list enrollments: %v %+v", err, enr.listed)
	}
	if _, _, err := f.CreateEnrollment(ctx, audit.Actor{}, EnrollmentInput{}); kindOf(t, err) != port.ActionErrorUnavailable {
		t.Fatalf("no creator: %v", err)
	}
	if _, err := f.ProlongEnrollment(ctx, audit.Actor{}, domain.WorkerEnrollmentID(uuid.New()), time.Hour); kindOf(t, err) != port.ActionErrorNotFound {
		t.Fatalf("prolong unknown: %v", err)
	}
	expires, err := f.ProlongEnrollment(ctx, audit.Actor{}, known, 2*MaxEnrollmentTTL)
	if err != nil || !expires.Equal(now.Add(MaxEnrollmentTTL)) {
		t.Fatalf("prolong caps at a year: %v %v", err, expires)
	}
	if (&Fleet{}).Workers(ctx) != nil {
		t.Fatal("no presence reader must yield nil")
	}
}
