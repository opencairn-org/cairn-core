package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	workerv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/worker/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

type fakePools struct{ m map[string]domain.WorkerPool }

func (f *fakePools) Upsert(_ context.Context, p domain.WorkerPool) error {
	k := p.Provider + "/" + p.Name
	if cur, ok := f.m[k]; ok {
		if cur.Package != p.Package {
			return domain.ErrPoolPackageChanged
		}
		return nil
	}
	f.m[k] = p
	return nil
}
func (f *fakePools) Get(_ context.Context, provider, name string) (domain.WorkerPool, error) {
	p, ok := f.m[provider+"/"+name]
	if !ok {
		return domain.WorkerPool{}, domain.ErrNotFound
	}
	return p, nil
}
func (f *fakePools) ListForProvider(_ context.Context, provider string) ([]domain.WorkerPool, error) {
	var out []domain.WorkerPool
	for _, p := range f.m {
		if p.Provider == provider {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakePools) ListAll(context.Context) ([]domain.WorkerPool, error) {
	var out []domain.WorkerPool
	for _, p := range f.m {
		out = append(out, p)
	}
	return out, nil
}
func (f *fakePools) Retire(_ context.Context, provider, name string) error {
	p, ok := f.m[provider+"/"+name]
	if !ok {
		return domain.ErrNotFound
	}
	now := time.Now()
	p.RetiredAt = &now
	f.m[provider+"/"+name] = p
	return nil
}

func TestKnownPools_SkipRetired(t *testing.T) {
	ctx := context.Background()
	pools := &fakePools{m: map[string]domain.WorkerPool{}}
	app := &App{WorkerPools: pools}
	_ = pools.Upsert(ctx, domain.WorkerPool{Provider: "garmin", Name: "old", Package: "a"})
	_ = pools.Upsert(ctx, domain.WorkerPool{Provider: "garmin", Name: "new", Package: "b"})
	if err := pools.Retire(ctx, "garmin", "old"); err != nil {
		t.Fatal(err)
	}
	if names := knownPoolNames(ctx, app, slog.Default(), "garmin"); len(names) != 1 || names[0] != "new" {
		t.Fatalf("known = %v", names)
	}
	if a, b := domain.AppID("client-1"), domain.AppID("client-1"); a != b || len(a) != 12 || domain.AppID("") != "" {
		t.Fatalf("AppID = %q %q", a, b)
	}
}

func TestCheckResultPool(t *testing.T) {
	ctx := context.Background()
	app := &App{WorkerPools: &fakePools{m: map[string]domain.WorkerPool{}}}
	log := slog.Default()
	jr := func(name, pkg string) *workerv1.JobResult {
		return &workerv1.JobResult{WorkerName: name, WorkerPackage: pkg}
	}
	// Pool-scoped subject registers the pool.
	if err := checkResultPool(ctx, app, log, "cairn.results.fetch_source.strava.w1", jr("w1", "pkg-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := app.WorkerPools.Get(ctx, "strava", "w1"); err != nil {
		t.Fatal("pool not registered")
	}
	// Another name on the subject than in the result.
	if err := checkResultPool(ctx, app, log, "cairn.results.fetch_source.strava.w1", jr("w2", "pkg-a")); err == nil {
		t.Fatal("name mismatch accepted")
	}
	// Reconcile carries the account after the pool.
	if err := checkResultPool(ctx, app, log, "cairn.results.reconcile.strava.w1.acct-1", jr("w1", "pkg-a")); err != nil {
		t.Fatal(err)
	}
	// The name is bound to its package.
	if err := checkResultPool(ctx, app, log, "cairn.results.fetch_source.strava.w1", jr("w1", "pkg-b")); !errors.Is(err, domain.ErrPoolPackageChanged) {
		t.Fatalf("package change: %v", err)
	}
	// Pre-0.5 provider-scoped subject: accepted, still registered.
	if err := checkResultPool(ctx, app, log, "cairn.results.fetch_source.garmin", jr("g1", "pkg-g")); err != nil {
		t.Fatal(err)
	}
	// Known pools come from the registry even with nothing online.
	if names := knownPoolNames(ctx, app, log, "strava"); len(names) != 1 || names[0] != "w1" {
		t.Fatalf("known pools = %v", names)
	}
}

func TestAccountPoolAllowed(t *testing.T) {
	assigned := domain.ExternalAccount{AssignedWorkerName: "garmin-api"}
	src := &domain.ActivitySource{SourceWorkerPackage: "pkg-api"}
	for _, c := range []struct {
		name, job, worker, pkg string
		acct                   domain.ExternalAccount
		src                    *domain.ActivitySource
		reject                 bool
	}{
		{"own pool imports", "fetch_source", "garmin-api", "pkg-api", assigned, nil, false},
		{"other pool imports", "fetch_source", "garmin-fit", "pkg-fit", assigned, nil, true},
		{"unassigned account", "fetch_source", "garmin-fit", "pkg-fit", domain.ExternalAccount{}, nil, false},
		{"legacy result without name", "fetch_source", "", "", assigned, nil, false},
		{"reparse same package other pool", "parse_blob", "garmin-api2", "pkg-api", assigned, src, false},
		{"reparse other package", "parse_blob", "garmin-fit", "pkg-fit", assigned, src, true},
		{"reparse unknown source", "parse_blob", "garmin-fit", "pkg-fit", assigned, nil, false},
	} {
		err := accountPoolAllowed(c.job, c.worker, c.pkg, c.acct, c.src)
		if errors.Is(err, ErrResultWrongPool) != c.reject {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}
