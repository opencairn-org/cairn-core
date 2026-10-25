package main

import (
	"context"
	"log/slog"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// presenceWorkerRouter implements port.WorkerRouter (docs/adr/0006): the
// account's assigned pool, else the provider's only live release-compatible
// pool, which then becomes the assignment.
type presenceWorkerRouter struct {
	app    *App
	logger *slog.Logger
}

func (r presenceWorkerRouter) AccountWorker(ctx context.Context, a domain.ExternalAccount) (string, error) {
	var live []string
	if a.AssignedWorkerName == "" {
		live = livePoolNames(currentWorkers(ctx, r.app, r.logger), a.Provider)
	}
	name, persist, err := domain.ResolveAccountWorker(a.AssignedWorkerName, live)
	if err != nil {
		return "", err
	}
	if persist && r.app.ExternalAccounts != nil {
		if err := r.app.ExternalAccounts.SetAssignedWorker(ctx, a.ID, name); err != nil {
			r.logger.Warn("persist worker assignment failed", "account_id", a.ID, "worker", name, "error", err)
		}
	}
	return name, nil
}

// livePoolNames lists the release-compatible pools online for provider.
func livePoolNames(workers []liveWorker, provider string) []string {
	var out []string
	for _, w := range workers {
		if w.compatible && w.provider == provider && w.name != "" {
			out = append(out, w.name)
		}
	}
	return out
}

// accountJobSubject routes an account-scoped job to the account's pool.
func accountJobSubject(ctx context.Context, app *App, logger *slog.Logger, job string, a domain.ExternalAccount) (string, error) {
	name, err := (presenceWorkerRouter{app: app, logger: logger}).AccountWorker(ctx, a)
	if err != nil {
		return "", err
	}
	return domain.JobSubject(job, a.Provider, name), nil
}
