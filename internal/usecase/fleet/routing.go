package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ErrResultWrongPool: an activity result from a pool that neither owns the
// account nor shares the source's package.
var ErrResultWrongPool = errors.New("result from a pool that does not serve this account")

// Routing knows a provider's pools (registry plus presence) and vets the
// results workers send back (docs/adr/0006). Every dependency is optional.
type Routing struct {
	Pools      port.WorkerPoolRepo
	Presence   port.WorkerPresence
	Accounts   port.ExternalAccountRepo
	Activities port.SourceReader
	Log        *slog.Logger
}

func (r *Routing) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Routing) live(ctx context.Context) []domain.LiveWorker {
	if r.Presence == nil {
		return nil
	}
	return r.Presence.LiveWorkers(ctx)
}

// Known lists a provider's unretired pools by name, with presence overlaid.
// A pool seen only in presence (its manifest not processed yet) is included.
// Compatible is the live instance's verdict, false while offline.
func (r *Routing) Known(ctx context.Context, provider string) []domain.WorkerPoolInfo {
	byName := r.registered(ctx, provider)
	for _, w := range r.live(ctx) {
		if w.Provider != provider || w.Name == "" {
			continue
		}
		p, ok := byName[w.Name]
		if !ok {
			if r.retired(ctx, provider, w.Name) {
				continue // online but not offered
			}
			p = &domain.WorkerPoolInfo{Name: w.Name, Package: w.Package}
			byName[w.Name] = p
		}
		p.Online, p.Compatible, p.ParserVersion, p.Release = true, w.Compatible, w.Version, w.Release
	}
	out := make([]domain.WorkerPoolInfo, 0, len(byName))
	for _, p := range byName {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// registered is the provider's unretired registry rows by name.
func (r *Routing) registered(ctx context.Context, provider string) map[string]*domain.WorkerPoolInfo {
	byName := map[string]*domain.WorkerPoolInfo{}
	if r.Pools == nil {
		return byName
	}
	rows, err := r.Pools.ListForProvider(ctx, provider)
	if err != nil {
		r.log().Warn("list worker pools failed", "provider", provider, "error", err)
	}
	for _, p := range rows {
		if p.RetiredAt != nil {
			continue // hidden from routing and pickers
		}
		byName[p.Name] = &domain.WorkerPoolInfo{Name: p.Name, Package: p.Package}
	}
	return byName
}

func (r *Routing) retired(ctx context.Context, provider, name string) bool {
	if r.Pools == nil {
		return false
	}
	reg, err := r.Pools.Get(ctx, provider, name)
	return err == nil && reg.RetiredAt != nil
}

// KnownNames is Known reduced to the pool names.
func (r *Routing) KnownNames(ctx context.Context, provider string) []string {
	pools := r.Known(ctx, provider)
	out := make([]string, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.Name)
	}
	return out
}

// CheckResultPool verifies a result against its pool: a pool-scoped subject
// must carry that pool's name, and the name stays bound to one package. A
// passing result registers or refreshes the pool.
func (r *Routing) CheckResultPool(ctx context.Context, subject, workerName, workerPackage string) error {
	_, provider, pool, ok := domain.ResultSubjectPool(subject)
	if !ok {
		return nil
	}
	if pool != "" && pool != workerName {
		return fmt.Errorf("result on %s claims pool %q", subject, workerName)
	}
	if r.Pools == nil || workerName == "" || workerPackage == "" {
		return nil
	}
	err := r.Pools.Upsert(ctx, domain.WorkerPool{Provider: provider, Name: workerName, Package: workerPackage})
	if errors.Is(err, domain.ErrPoolPackageChanged) {
		return err
	}
	if err != nil {
		r.log().Warn("register worker pool failed", "provider", provider, "pool", workerName, "error", err)
	}
	return nil
}

// ResultRef identifies the activity a result is about.
type ResultRef struct {
	Provider, ExternalAccountID, ExternalID string
}

// CheckResultAccount loads what AccountPoolAllowed needs; lookups that fail
// transiently do not reject the result.
func (r *Routing) CheckResultAccount(ctx context.Context, job, workerName, workerPackage string, ref ResultRef) error {
	if ref.ExternalAccountID == "" || r.Accounts == nil {
		return nil
	}
	acctUUID, err := uuid.Parse(ref.ExternalAccountID)
	if err != nil {
		return nil // ingest reports the malformed ref
	}
	acctID := domain.ExternalAccountID(acctUUID)
	acct, err := r.Accounts.GetExternalAccount(ctx, acctID)
	if err != nil {
		return nil
	}
	var src *domain.ActivitySource
	if job == "parse_blob" && r.Activities != nil {
		if s, err := r.Activities.FindSourceByExternalID(ctx, ref.Provider, &acctID, ref.ExternalID); err == nil {
			src = &s
		}
	}
	return AccountPoolAllowed(job, workerName, workerPackage, acct, src)
}

// AccountPoolAllowed decides whether a pool's activity result is accepted.
// fetch_source/backfill results must come from the account's pool (an
// unassigned account accepts any). parse_blob results must come from the
// package that imported the source, since re-parse is routed by package.
func AccountPoolAllowed(job, workerName, workerPackage string, acct domain.ExternalAccount, src *domain.ActivitySource) error {
	switch job {
	case "parse_blob":
		if src != nil && src.SourceWorkerPackage != "" && src.SourceWorkerPackage != workerPackage {
			return fmt.Errorf("%w: re-parse by package %s of a %s source", ErrResultWrongPool, workerPackage, src.SourceWorkerPackage)
		}
	default:
		if acct.AssignedWorkerName != "" && workerName != "" && acct.AssignedWorkerName != workerName {
			return fmt.Errorf("%w: account is served by %s, result from %s", ErrResultWrongPool, acct.AssignedWorkerName, workerName)
		}
	}
	return nil
}

// DegradedTransitions returns the live workers that just became
// release-incompatible and the set to carry into the next tick.
func DegradedTransitions(prev map[string]bool, workers []domain.LiveWorker) (newly []domain.LiveWorker, next map[string]bool) {
	next = map[string]bool{}
	for _, w := range workers {
		if w.Compatible {
			continue
		}
		key := w.Provider + "/" + w.Name
		next[key] = true
		if !prev[key] {
			newly = append(newly, w)
		}
	}
	return newly, next
}

// DegradedEvent is the operator event for a worker entering the degraded state.
func DegradedEvent(w domain.LiveWorker, coreRelease string) domain.AdminEvent {
	return domain.NewAdminEvent(domain.AdminEventWorkerDegraded, domain.AuditWarn, "admin.worker_degraded").
		AboutWorker(w.Name).With("provider", w.Provider).With("release", w.Release).With("core_release", coreRelease)
}
