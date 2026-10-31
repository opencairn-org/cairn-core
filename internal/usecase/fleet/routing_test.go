package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type registry struct {
	port.WorkerPoolRepo
	m map[string]domain.WorkerPool
}

func (f *registry) Upsert(_ context.Context, p domain.WorkerPool) error {
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

func (f *registry) Get(_ context.Context, provider, name string) (domain.WorkerPool, error) {
	p, ok := f.m[provider+"/"+name]
	if !ok {
		return domain.WorkerPool{}, domain.ErrNotFound
	}
	return p, nil
}

func (f *registry) ListForProvider(_ context.Context, provider string) ([]domain.WorkerPool, error) {
	var out []domain.WorkerPool
	for _, p := range f.m {
		if p.Provider == provider {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *registry) retire(provider, name string) {
	p := f.m[provider+"/"+name]
	now := time.Now()
	p.RetiredAt = &now
	f.m[provider+"/"+name] = p
}

type presence []domain.LiveWorker

func (p presence) LiveWorkers(context.Context) []domain.LiveWorker { return p }

func TestRoutingKnown(t *testing.T) {
	ctx := context.Background()
	reg := &registry{m: map[string]domain.WorkerPool{}}
	_ = reg.Upsert(ctx, domain.WorkerPool{Provider: "garmin", Name: "old", Package: "a"})
	_ = reg.Upsert(ctx, domain.WorkerPool{Provider: "garmin", Name: "new", Package: "b"})
	_ = reg.Upsert(ctx, domain.WorkerPool{Provider: "garmin", Name: "gone", Package: "c"})
	reg.retire("garmin", "old")
	reg.retire("garmin", "gone")
	live := presence{
		{Name: "new", Provider: "garmin", Version: "2.1", Release: "v1.0.0", Package: "b", Compatible: true},
		{Name: "fresh", Provider: "garmin", Version: "1.0", Package: "d"},  // manifest not processed yet
		{Name: "gone", Provider: "garmin", Package: "c", Compatible: true}, // retired, still online
		{Name: "s1", Provider: "strava", Package: "s"},
	}
	r := &Routing{Pools: reg, Presence: live}
	got := r.Known(ctx, "garmin")
	if len(got) != 2 || got[0].Name != "fresh" || got[1].Name != "new" {
		t.Fatalf("known = %+v", got)
	}
	if n := got[1]; !n.Online || !n.Compatible || n.ParserVersion != "2.1" || n.Release != "v1.0.0" || n.Package != "b" {
		t.Fatalf("overlay = %+v", n)
	}
	if f := got[0]; !f.Online || f.Compatible || f.Package != "d" {
		t.Fatalf("presence-only = %+v", f)
	}
	if names := (&Routing{Pools: reg}).KnownNames(ctx, "garmin"); len(names) != 1 || names[0] != "new" {
		t.Fatalf("registry only = %v", names)
	}
	if names := (&Routing{}).KnownNames(ctx, "garmin"); len(names) != 0 {
		t.Fatalf("nothing wired = %v", names)
	}
}

func TestRoutingCheckResultPool(t *testing.T) {
	ctx := context.Background()
	reg := &registry{m: map[string]domain.WorkerPool{}}
	r := &Routing{Pools: reg}
	// Pool-scoped subject registers the pool.
	if err := r.CheckResultPool(ctx, "cairn.results.fetch_source.strava.w1", "w1", "pkg-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get(ctx, "strava", "w1"); err != nil {
		t.Fatal("pool not registered")
	}
	// Another name on the subject than in the result.
	if err := r.CheckResultPool(ctx, "cairn.results.fetch_source.strava.w1", "w2", "pkg-a"); err == nil {
		t.Fatal("name mismatch accepted")
	}
	// Reconcile carries the account after the pool.
	if err := r.CheckResultPool(ctx, "cairn.results.reconcile.strava.w1.acct-1", "w1", "pkg-a"); err != nil {
		t.Fatal(err)
	}
	// The name is bound to its package.
	if err := r.CheckResultPool(ctx, "cairn.results.fetch_source.strava.w1", "w1", "pkg-b"); !errors.Is(err, domain.ErrPoolPackageChanged) {
		t.Fatalf("package change: %v", err)
	}
	// Pre-0.5 provider-scoped subject: accepted, still registered.
	if err := r.CheckResultPool(ctx, "cairn.results.fetch_source.garmin", "g1", "pkg-g"); err != nil {
		t.Fatal(err)
	}
	// Known pools come from the registry even with nothing online.
	if names := r.KnownNames(ctx, "strava"); len(names) != 1 || names[0] != "w1" {
		t.Fatalf("known pools = %v", names)
	}
	// Kept from the cmd/server pool test.
	if a, b := domain.AppID("client-1"), domain.AppID("client-1"); a != b || len(a) != 12 || domain.AppID("") != "" {
		t.Fatalf("AppID = %q %q", a, b)
	}
}

type accounts struct {
	port.ExternalAccountRepo
	acct domain.ExternalAccount
}

func (a accounts) GetExternalAccount(context.Context, domain.ExternalAccountID) (domain.ExternalAccount, error) {
	return a.acct, nil
}

type sources struct {
	port.ActivityRepo
	src domain.ActivitySource
}

func (s sources) FindSourceByExternalID(context.Context, string, *domain.ExternalAccountID, string) (domain.ActivitySource, error) {
	return s.src, nil
}

func TestRoutingCheckResultAccount(t *testing.T) {
	ctx := context.Background()
	r := &Routing{Accounts: accounts{acct: domain.ExternalAccount{AssignedWorkerName: "garmin-api"}}, Activities: sources{src: domain.ActivitySource{SourceWorkerPackage: "pkg-api"}}}
	ref := ResultRef{Provider: "garmin", ExternalAccountID: "6f1c1b7e-8d0e-4c43-9a51-6a1f0e3b2d10", ExternalID: "1"}
	if err := r.CheckResultAccount(ctx, "fetch_source", "garmin-fit", "pkg-fit", ref); !errors.Is(err, ErrResultWrongPool) {
		t.Fatalf("other pool: %v", err)
	}
	if err := r.CheckResultAccount(ctx, "parse_blob", "garmin-fit", "pkg-api", ref); err != nil {
		t.Fatalf("same package re-parse: %v", err)
	}
	if err := r.CheckResultAccount(ctx, "parse_blob", "garmin-api", "pkg-fit", ref); !errors.Is(err, ErrResultWrongPool) {
		t.Fatalf("other package re-parse: %v", err)
	}
	ref.ExternalAccountID = "not-a-uuid"
	if err := r.CheckResultAccount(ctx, "fetch_source", "garmin-fit", "pkg-fit", ref); err != nil {
		t.Fatalf("malformed ref: %v", err)
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
		err := AccountPoolAllowed(c.job, c.worker, c.pkg, c.acct, c.src)
		if errors.Is(err, ErrResultWrongPool) != c.reject {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

func TestDegradedTransitions(t *testing.T) {
	workers := []domain.LiveWorker{{Name: "a", Provider: "strava", Compatible: true}, {Name: "b", Provider: "strava", Release: "v0.1.0"}}
	newly, next := DegradedTransitions(nil, workers)
	if len(newly) != 1 || newly[0].Name != "b" || !next["strava/b"] || next["strava/a"] {
		t.Fatalf("first tick: %+v %v", newly, next)
	}
	if again, _ := DegradedTransitions(next, workers); len(again) != 0 {
		t.Fatalf("already reported: %+v", again)
	}
	workers[1].Compatible = true
	if _, next = DegradedTransitions(next, workers); len(next) != 0 {
		t.Fatalf("recovered worker dropped: %v", next)
	}
	ev := DegradedEvent(newly[0], "v1.2.0")
	if ev.Type != domain.AdminEventWorkerDegraded || ev.Params["core_release"] != "v1.2.0" || ev.Params["release"] != "v0.1.0" {
		t.Fatalf("event = %+v", ev)
	}
}
