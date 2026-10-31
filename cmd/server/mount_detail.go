package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/staticmap"
	"github.com/opencairn-org/cairn-core/internal/usecase/attachment"
	"github.com/opencairn-org/cairn-core/internal/usecase/content"
	"github.com/opencairn-org/cairn-core/internal/usecase/mapimage"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// detailAccess is the activity loader of the detail handlers.
func detailAccess(app *App) *view.ActivityAccess {
	return view.NewActivityAccess(app.Activities, app.Visibility2)
}

// detailMapImages builds the course-image renderer over the configured basemap.
func detailMapImages(app *App, cfg config.MapConfig, logger *slog.Logger) *mapimage.Images {
	ua := cfg.UserAgent
	if ua == "" {
		ua = "Cairn/" + Version + " (static map; +https://github.com/opencairn-org/cairn-core)"
	}
	var zones mapimage.ZoneSource
	if app.Visibility2 != nil {
		zones = app.Visibility2
	}
	var streams port.StreamRepo // a nil *postgres.StreamRepo must stay a nil interface
	if app.Streams != nil {
		streams = app.Streams
	}
	return mapimage.New(streams, app.BlobStore, zones, staticmap.HTTPTileFetcher(cfg.TileURL, ua, cfg.Timeout), logger)
}

// mountContent mounts the signed /content/ route and installs app.Content,
// which the REST and Connect handlers mounted after it hand out URLs with.
func mountContent(mux *http.ServeMux, app *App, cfg *config.Config, logger *slog.Logger) {
	signer := content.NewSigner(content.NewCodec(cfg.Auth.SessionSecret, nil), cfg.HTTP.PublicBaseURL, cfg.Storage.PresignTTL, app.BlobStore)
	app.Content = signer
	(&rest.ContentHandlers{
		Signer:  signer,
		Access:  detailAccess(app),
		Images:  detailMapImages(app, cfg.Map, logger),
		Exports: app.Exports,
		Log:     logger,
	}).Mount(mux)
	logger.Info("signed content route mounted", "path", "/content/{token}", "public_endpoint", cfg.Storage.PublicEndpoint != "")
}

// mountDetail mounts the activity detail reads, exports, the course image and photos.
func mountDetail(mux *http.ServeMux, app *App, cfg *config.Config, logger *slog.Logger) {
	auth, access := restAuth(app), detailAccess(app)
	(&rest.DetailHandlers{
		Auth: auth, Access: access, Details: app.Details, Exports: app.Exports, Log: logger,
		Streams: app.Streams != nil, Segments: app.Segments != nil,
	}).Mount(mux)
	(&rest.MapImageHandlers{Auth: auth, Access: access, Shares: app.Shares, Images: detailMapImages(app, cfg.Map, logger)}).Mount(mux)
	logger.Info("activity detail endpoints mounted", "tiles", cfg.Map.TileURL)
	if app.Attachments == nil {
		return
	}
	photos := attachment.NewPhotos(app.Attachments, app.BlobStore, app.AttachmentVariants, app.Content, logger)
	(&rest.AttachmentHandlers{Auth: auth, Access: access, Shares: app.Shares, Photos: photos}).Mount(mux)
	logger.Info("attachment endpoints mounted", "path", "/api/activities/{id}/attachments")
}
