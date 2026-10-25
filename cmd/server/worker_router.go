package main

import (
	"context"
	"log/slog"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// presenceWorkerRouter implements port.WorkerRouter (docs/adr/0006): the
// account's assigned pool, else the provider's only known pool, which then
// becomes the assignment.
type presenceWorkerRouter struct {
	app    *App
	logger *slog.Logger
}

func (r presenceWorkerRouter) AccountWorker(ctx context.Context, a domain.ExternalAccount) (string, error) {
	var known []string
	if a.AssignedWorkerName == "" {
		known = knownPoolNames(ctx, r.app, r.logger, a.Provider)
	}
	name, persist, err := domain.ResolveAccountWorker(a.AssignedWorkerName, known)
	if err != nil {
		return "", err
	}
	// Persist so the account keeps its pool; failing (e.g. a sibling
	// connection already holds it) stops the job rather than send it twice.
	if persist && r.app.ExternalAccounts != nil {
		if err := r.app.ExternalAccounts.SetAssignedWorker(ctx, a.ID, name); err != nil {
			return "", err
		}
	}
	return name, nil
}

// accountJobSubject routes an account-scoped job to the account's pool.
func accountJobSubject(ctx context.Context, app *App, logger *slog.Logger, job string, a domain.ExternalAccount) (string, error) {
	name, err := (presenceWorkerRouter{app: app, logger: logger}).AccountWorker(ctx, a)
	if err != nil {
		return "", err
	}
	return domain.JobSubject(job, a.Provider, name), nil
}
