package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// restAuth is the REST adapter's view of the credential resolver.
func restAuth(app *App) *rest.Auth { return &rest.Auth{Callers: app.Callers} }

// resolveSessionUser authenticates a request for the handlers still in this
// package; it is the same resolver the rest adapter uses.
func resolveSessionUser(r *http.Request, app *App) (domain.UserID, bool) {
	return restAuth(app).User(r)
}

// shareTokenFor reports whether the request's ?share= token opens actID.
func shareTokenFor(r *http.Request, app *App, actID domain.ActivityID) bool {
	return app.Shares.Grants(r.Context(), r.URL.Query().Get("share"), actID)
}

func mountRestShare(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.ShareLinks == nil {
		return
	}
	(&rest.ShareHandlers{Auth: restAuth(app), Links: app.Shares, Feeds: app.Feeds, Log: logger}).Mount(mux)
	logger.Info("share-link endpoints mounted")
}
