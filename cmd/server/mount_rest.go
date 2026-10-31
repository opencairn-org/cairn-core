package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
)

// restAuth is the REST adapter's view of the credential resolver.
func restAuth(app *App) *rest.Auth { return &rest.Auth{Callers: app.Callers} }

func mountRestShare(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.ShareLinks == nil {
		return
	}
	(&rest.ShareHandlers{Auth: restAuth(app), Links: app.Shares, Feeds: app.Feeds, Log: logger}).Mount(mux)
	logger.Info("share-link endpoints mounted")
}
