package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/usecase/jobs"
)

// mountRestAdmin mounts the session+admin gated /api/admin/* surface.
func mountRestAdmin(mux *http.ServeMux, app *App, logger *slog.Logger) {
	auth := restAuth(app)
	if app.Fleet == nil || app.CreateWorkerEnrollment == nil || app.WorkerEnrollments == nil {
		logger.Info("admin api not mounted: worker enrollment not wired")
	} else {
		(&rest.AdminFleetHandlers{Auth: auth, Fleet: app.Fleet, CoreRelease: Version, Log: logger}).Mount(mux)
		logger.Info("admin api endpoints mounted (session+admin gated)")
	}
	if app.UserAdmin != nil {
		(&rest.AdminUserHandlers{Auth: auth, Users: app.UserAdmin, Audit: app.AuditLog, PublicBaseURL: app.PublicBaseURL}).Mount(mux)
	}
	(&rest.AdminInstanceHandlers{Auth: auth, Settings: app.InstanceSettings, TestEmail: app.TestEmail, Health: app.SystemHealth, Invites: app.InviteAdmin}).Mount(mux)
}

// mountRestAdminOps mounts the operator maintenance endpoints.
func mountRestAdminOps(mux *http.ServeMux, app *App, logger *slog.Logger) {
	// newAdminServer builds an equal one later; both belong in wire.go.
	if app.Orphans == nil {
		app.Orphans = &jobs.Orphans{Activities: app.Activities, LiveWorkers: liveWorkerNames(app, logger)}
		if app.WorkerPools != nil {
			app.Orphans.Pools = app.WorkerPools
		}
	}
	(&rest.AdminOpsHandlers{Auth: restAuth(app), Ops: app.Ops, Jobs: app.Jobs, Orphans: app.Orphans,
		WebhookTokens: app.WebhookTokens, Maintenance: app.Maintenance}).Mount(mux)
	logger.Info("admin ops endpoints mounted (/api/admin/{geocode,nats,dlq})")
}

// mountRestHealthMetrics serves merged daily health metrics; empty until a health worker imports.
func mountRestHealthMetrics(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Health == nil {
		return
	}
	(&rest.HealthMetricsHandlers{Auth: restAuth(app), Health: app.HealthViews, Log: logger}).Mount(mux)
}

func mountRestInstanceFeatures(mux *http.ServeMux, cfg *config.Config, logger *slog.Logger) {
	(&rest.InstanceFeaturesHandlers{Federation: cfg.Federation.Enabled}).Mount(mux)
	logger.Info("instance features endpoint mounted", "federation", cfg.Federation.Enabled)
}
