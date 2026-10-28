package main

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// mountActivityExport serves the merged activity as a standard interchange
// file (GPX / TCX), generated from Cairn's own merged stream + summary — so it
// works regardless of the original provider/format. Distinct from the
// per-source "download original" which streams the archived raw blob verbatim.
//
// The optional ?exclude=gps,heart_rate,… query drops features from the file
// (tokens from export.OptionsFromExclude); the options endpoint tells the UI
// which features this activity actually has.
//
//	GET /api/activities/{id}/export.gpx
//	GET /api/activities/{id}/export.tcx
//	GET /api/activities/{id}/export/options
func mountActivityExport(mux *http.ServeMux, app *App, logger *slog.Logger) {
	mux.HandleFunc("GET /api/activities/{id}/export.gpx", activityExportFileHandler(app, logger, "gpx"))
	mux.HandleFunc("GET /api/activities/{id}/export.tcx", activityExportFileHandler(app, logger, "tcx"))
	mux.HandleFunc("GET /api/activities/{id}/export.fit", activityExportFileHandler(app, logger, "fit"))

	mux.HandleFunc("GET /api/activities/{id}/export/options", func(w http.ResponseWriter, r *http.Request) {
		act, ok := activityExportOwned(app, w, r)
		if !ok {
			return
		}
		available, err := app.Exports.Available(r.Context(), act)
		if err != nil {
			writeActionError(w, err, "export options failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"available": available})
	})

	logger.Info("activity export endpoints mounted")
}

// activityExportOwned resolves auth + the caller's activity; on failure it
// has already written the HTTP error.
func activityExportOwned(app *App, w http.ResponseWriter, r *http.Request) (domain.Activity, bool) {
	userID, ok := resolveSessionUser(r, app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return domain.Activity{}, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad activity id", http.StatusBadRequest)
		return domain.Activity{}, false
	}
	act, err := app.Activities.GetActivity(r.Context(), domain.ActivityID(id))
	if err != nil || act.UserID != userID || act.IsDeleted() {
		http.Error(w, "not found", http.StatusNotFound)
		return domain.Activity{}, false
	}
	return act, true
}

func activityExportFileHandler(app *App, logger *slog.Logger, format string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		act, ok := activityExportOwned(app, w, r)
		if !ok {
			return
		}
		file, err := app.Exports.Render(r.Context(), act, format, r.URL.Query().Get("exclude"))
		if err != nil {
			var ae *port.ActionError
			if !errors.As(err, &ae) {
				logger.Error("export failed", "activity", act.ID, "format", format, "error", err)
			}
			writeActionError(w, err, "export failed")
			return
		}
		w.Header().Set("Content-Type", file.ContentType)
		w.Header().Set("Content-Disposition", "attachment; filename=\""+file.Filename+"\"")
		w.Header().Set("Content-Length", strconv.Itoa(len(file.Data)))
		_, _ = w.Write(file.Data) //nolint:gosec // G705: non-HTML content type served as an attachment
	}
}
