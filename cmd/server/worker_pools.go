package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	workerv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/worker/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// poolInfo is one known worker pool of a provider (docs/adr/0006): the
// registry row plus what presence says right now.
type poolInfo struct {
	name, pkg, parser, release string
	online, compatible         bool
}

// knownPools lists a provider's pools from the registry, with presence
// overlaid. A pool seen only in presence (its manifest not processed yet) is
// included too.
func knownPools(ctx context.Context, app *App, logger *slog.Logger, provider string) []poolInfo {
	byName := map[string]*poolInfo{}
	if app.WorkerPools != nil {
		rows, err := app.WorkerPools.ListForProvider(ctx, provider)
		if err != nil {
			logger.Warn("list worker pools failed", "provider", provider, "error", err)
		}
		for _, p := range rows {
			if p.RetiredAt != nil {
				continue // hidden from routing and pickers
			}
			byName[p.Name] = &poolInfo{name: p.Name, pkg: p.Package}
		}
	}
	for _, w := range currentWorkers(ctx, app, logger) {
		if w.provider != provider || w.name == "" {
			continue
		}
		p, ok := byName[w.name]
		if !ok {
			if app.WorkerPools != nil {
				if reg, err := app.WorkerPools.Get(ctx, provider, w.name); err == nil && reg.RetiredAt != nil {
					continue // retired: online but not offered
				}
			}
			p = &poolInfo{name: w.name, pkg: w.pkg}
			byName[w.name] = p
		}
		p.online, p.compatible, p.parser, p.release = true, w.compatible, w.version, w.release
	}
	out := make([]poolInfo, 0, len(byName))
	for _, p := range byName {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func knownPoolNames(ctx context.Context, app *App, logger *slog.Logger, provider string) []string {
	pools := knownPools(ctx, app, logger, provider)
	out := make([]string, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.name)
	}
	return out
}

// workerPoolsJSON renders a provider's known pools for the UI pickers.
func workerPoolsJSON(ctx context.Context, app *App, logger *slog.Logger, provider string) []map[string]any {
	out := []map[string]any{}
	for _, p := range knownPools(ctx, app, logger, provider) {
		out = append(out, map[string]any{
			"name": p.name, "package": p.pkg, "parser_version": p.parser, "release": p.release,
			"online": p.online, "compatible": !p.online || p.compatible,
		})
	}
	return out
}

// checkResultPool verifies a result against its pool (docs/adr/0006): a
// pool-scoped subject must carry that pool's name, and the name must stay
// bound to one package. A passing result registers or refreshes the pool.
func checkResultPool(ctx context.Context, app *App, log *slog.Logger, subject string, jr *workerv1.JobResult) error {
	_, provider, pool, ok := domain.ResultSubjectPool(subject)
	if !ok {
		return nil
	}
	name := jr.GetWorkerName()
	if pool != "" && pool != name {
		return fmt.Errorf("result on %s claims pool %q", subject, name)
	}
	if app.WorkerPools == nil || name == "" || jr.GetWorkerPackage() == "" {
		return nil
	}
	err := app.WorkerPools.Upsert(ctx, domain.WorkerPool{Provider: provider, Name: name, Package: jr.GetWorkerPackage()})
	if errors.Is(err, domain.ErrPoolPackageChanged) {
		return err
	}
	if err != nil {
		log.Warn("register worker pool failed", "provider", provider, "pool", name, "error", err)
	}
	return nil
}

// presenceCache bounds presence-KV scans: routing, pickers and lists ask
// for live workers far more often than presence changes.
type presenceCache struct {
	mu   sync.Mutex
	at   time.Time
	list []liveWorker
}

const presenceCacheTTL = 5 * time.Second

func (c *presenceCache) get(now time.Time, scan func() []liveWorker) []liveWorker {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.at) < presenceCacheTTL {
		return c.list
	}
	c.list, c.at = scan(), now
	return c.list
}

// ErrResultWrongPool: an activity result from a pool that neither owns the
// account nor shares the source's package.
var ErrResultWrongPool = errors.New("result from a pool that does not serve this account")

// accountPoolAllowed decides whether a pool's activity result is accepted
// (docs/adr/0006). fetch_source/backfill results must come from the account's
// pool (an unassigned account accepts any). parse_blob results must come from
// the package that imported the source, since re-parse is routed by package.
func accountPoolAllowed(job, workerName, workerPackage string, acct domain.ExternalAccount, src *domain.ActivitySource) error {
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

// checkResultAccount loads what accountPoolAllowed needs; lookups that fail
// transiently do not reject the result.
func checkResultAccount(ctx context.Context, app *App, job string, jr *workerv1.JobResult, ia *workerv1.ImportedActivity) error {
	ref := ia.GetRef()
	if ref.GetExternalAccountId() == "" || app.ExternalAccounts == nil {
		return nil
	}
	acctUUID, err := uuid.Parse(ref.GetExternalAccountId())
	if err != nil {
		return nil // ingest reports the malformed ref
	}
	acctID := domain.ExternalAccountID(acctUUID)
	acct, err := app.ExternalAccounts.GetExternalAccount(ctx, acctID)
	if err != nil {
		return nil
	}
	var src *domain.ActivitySource
	if job == "parse_blob" && app.Activities != nil {
		if s, err := app.Activities.FindSourceByExternalID(ctx, ref.GetProvider(), &acctID, ref.GetExternalId()); err == nil {
			src = &s
		}
	}
	return accountPoolAllowed(job, jr.GetWorkerName(), jr.GetWorkerPackage(), acct, src)
}
