package jobs

import (
	"context"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// OrphanGroup is the sources expecting a worker that is not around.
type OrphanGroup struct {
	WorkerName       string
	Sources          int
	ExternalAccounts int
	Users            int
	OldestImportedAt *time.Time
}

// Orphans finds sources whose worker pool is gone: not live and either
// unknown or retired.
type Orphans struct {
	Activities port.ActivityRepo
	Pools      port.WorkerPoolRepo
	// LiveWorkers returns the names of the worker pools currently online.
	LiveWorkers func(ctx context.Context) []string
}

// List groups orphaned sources by worker name; workerName narrows to one.
func (o *Orphans) List(ctx context.Context, workerName string) ([]OrphanGroup, error) {
	if o.Activities == nil {
		return nil, port.NewActionError(port.ActionErrorUnavailable, "sources are not available")
	}
	groups, err := o.Activities.SourceWorkerGroups(ctx)
	if err != nil {
		return nil, internalErr("group sources by worker", err)
	}
	present := o.presentWorkers(ctx)
	out := []OrphanGroup{}
	for _, g := range groups {
		if g.WorkerName == "" || present[g.WorkerName] || (workerName != "" && g.WorkerName != workerName) {
			continue
		}
		out = append(out, OrphanGroup{WorkerName: g.WorkerName, Sources: g.Sources, ExternalAccounts: g.Accounts, Users: g.Users, OldestImportedAt: g.OldestImportedAt})
	}
	return out, nil
}

// presentWorkers is the set of pool names that are live or still registered.
func (o *Orphans) presentWorkers(ctx context.Context) map[string]bool {
	present := map[string]bool{}
	if o.LiveWorkers != nil {
		for _, n := range o.LiveWorkers(ctx) {
			present[n] = true
		}
	}
	if o.Pools == nil {
		return present
	}
	pools, err := o.Pools.ListAll(ctx)
	if err != nil {
		return present
	}
	for _, p := range pools {
		if p.RetiredAt == nil {
			present[p.Name] = true
		}
	}
	return present
}

var _ = domain.ErrNotFound
