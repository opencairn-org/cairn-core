package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

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
