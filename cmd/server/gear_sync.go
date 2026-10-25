package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/capability"
)

// Gear sync: core asks a provider worker for the account's full gear list
// (cairn.jobs.import_gear.<provider>, docs/gear-v0.4.0.md). Published on
// account connect, on the user's "Sync gear" action and alongside every
// reconcile — but only when a live worker for the provider declares the
// Gear data type, because CAIRN_JOBS is a work queue and a job nobody
// consumes would sit there forever.

var errGearNotSupported = errors.New("no connected worker for this provider imports gear")

// workerDeclares reports whether any live worker for provider can read dt.
func workerDeclares(ctx context.Context, app *App, logger *slog.Logger, provider string, dt capability.DataType) bool {
	for _, w := range currentWorkers(ctx, app, logger) {
		if w.compatible && w.provider == provider && w.capabilities.CanRead(dt) {
			return true
		}
	}
	return false
}

// importGearJobBody is the JSON job a worker receives on import_gear.
type importGearJobBody struct {
	JobID     string    `json:"job_id"`
	AccountID string    `json:"account_id"`
	UserID    string    `json:"user_id"`
	Provider  string    `json:"provider"`
	Reason    string    `json:"reason"`
	IssuedAt  time.Time `json:"issued_at"`
}

// publishImportGear enqueues an import_gear job for the account. Returns
// errGearNotSupported when no live worker for the provider declares gear.
func publishImportGear(ctx context.Context, app *App, logger *slog.Logger, acct domain.ExternalAccount, reason string) error {
	if app.NATSBus == nil {
		return errors.New("worker bus unavailable")
	}
	if !workerDeclares(ctx, app, logger, acct.Provider, capability.DataTypeGear) {
		return errGearNotSupported
	}
	now := time.Now().UTC()
	body, err := json.Marshal(importGearJobBody{
		JobID:     uuid.NewString(),
		AccountID: acct.ID.String(),
		UserID:    acct.UserID.String(),
		Provider:  acct.Provider,
		Reason:    reason,
		IssuedAt:  now,
	})
	if err != nil {
		return err
	}
	// Minute-bucketed like reconcile: a tick and a manual trigger within the
	// same minute collapse to one job.
	msgID := fmt.Sprintf("import_gear:%s:%s", acct.ID, now.Format("2006-01-02T15:04"))
	return app.NATSBus.Publish(ctx, "cairn.jobs.import_gear."+acct.Provider, msgID, body)
}

// gearSyncHook is the reconcile-time publisher; a provider whose worker does
// not import gear is simply skipped.
func gearSyncHook(app *App, logger *slog.Logger) func(ctx context.Context, a domain.ExternalAccount) error {
	return func(ctx context.Context, a domain.ExternalAccount) error {
		err := publishImportGear(ctx, app, logger, a, "scheduled")
		if errors.Is(err, errGearNotSupported) {
			return nil
		}
		return err
	}
}

// syncGearOnConnect publishes the first gear import for a freshly linked
// account. Best-effort: the next reconcile repeats it.
func syncGearOnConnect(ctx context.Context, app *App, logger *slog.Logger, acctID domain.ExternalAccountID) {
	if app.ExternalAccounts == nil {
		return
	}
	acct, err := app.ExternalAccounts.GetExternalAccount(ctx, acctID)
	if err != nil {
		return
	}
	if err := publishImportGear(ctx, app, logger, acct, "connect"); err != nil && !errors.Is(err, errGearNotSupported) {
		logger.Warn("gear sync on connect failed", "account_id", acctID, "error", err)
	}
}

// mountGearSync serves POST /api/accounts/{id}/sync-gear for the connections
// page (same shape as the Garmin health import trigger).
func mountGearSync(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.ExternalAccounts == nil {
		return
	}
	mux.HandleFunc("POST /api/accounts/{id}/sync-gear", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := resolveSessionUser(r, app)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		accID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad account id", http.StatusBadRequest)
			return
		}
		acct, err := app.ExternalAccounts.GetExternalAccount(r.Context(), domain.ExternalAccountID(accID))
		if err != nil || acct.UserID != userID {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		switch err := publishImportGear(r.Context(), app, logger, acct, "manual"); {
		case errors.Is(err, errGearNotSupported):
			http.Error(w, err.Error(), http.StatusConflict)
		case err != nil:
			http.Error(w, "enqueue failed", http.StatusInternalServerError)
		default:
			writeJSON(w, http.StatusOK, map[string]any{"queued": true})
		}
	})
}
