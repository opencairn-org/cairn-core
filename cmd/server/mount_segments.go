package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

func mountRestSegments(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.SegmentViews == nil {
		return
	}
	h := &rest.SegmentHandlers{
		Auth: restAuth(app), Views: app.SegmentViews, Authoring: app.SegmentAuthoring, Details: app.Details,
		Activities: app.SourceActions, Log: logger,
		Backfill: func(userID domain.UserID, id domain.SegmentID, from, to time.Time) string {
			return backfillSegment(app, logger, userID, id, from, to)
		},
	}
	// The library and activity efforts need the segment repo; browsing reports ErrUnavailable instead.
	if app.Segments != nil {
		h.MountMine(mux)
	}
	h.Mount(mux)
	logger.Info("segment endpoints mounted")
}
