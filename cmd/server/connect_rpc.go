package main

import (
	"log/slog"
	"net/http"

	connectrpc "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	connectadapter "github.com/opencairn-org/cairn-core/internal/adapter/primary/connect"
)

// mountConnectRPC registers every cairn.v1.* Connect-RPC service the
// server currently implements onto the supplied mux.
//
// Service handlers come out of internal/adapter/primary/connect. Each
// embeds the generated UnimplementedXxxServiceHandler so the surface
// grows method-by-method; clients calling an unimplemented RPC get
// CodeUnimplemented, not a 404.
//
// Auth is currently header-based (X-Cairn-User-ID), set by the SvelteKit
// hooks.server.ts. The proper session-cookie / OIDC path replaces that
// header check once OIDC adapters land.
func mountConnectRPC(mux *http.ServeMux, app *App, logger *slog.Logger) {
	// Session interceptor: resolves the cairn_session cookie into a
	// context-stashed user id. Handlers read via userIDFromHandlerCtx;
	// the X-Cairn-User-ID header path stays as a fallback so the
	// SvelteKit dev proxy can keep working during the OIDC roll-out.
	sessionInterceptor := connectadapter.SessionInterceptor(app.Sessions, app.PATs, app.OAuth, sessionCookieName)
	// ScopeInterceptor keeps read-only OAuth tokens read-only; no-op for
	// cookie/PAT callers. Runs after the session resolver.
	server := connectrpc.NewServer(sessionInterceptor, connectadapter.ScopeInterceptor())

	activitySvc := connectadapter.NewActivityServer(app.Activities, app.Streams, app.BestEfforts, app.RecomputeActivity,
		app.ClassOverrides, newFederationPublisher(app, logger), app.Gear)
	// Source actions, uploads and re-imports share their implementation with
	// the REST handlers so the two surfaces stay in parity.
	activitySvc.SourceActions = app.SourceActions
	activitySvc.SourceLinks = app.SourceLinks
	activitySvc.Reimporter = newSourceReimporter(app, logger)
	activitySvc.Uploads = &uploadAPI{app: app, logger: logger}
	activitySvc.Blobs = app.BlobStore
	cairnv1connect.RegisterActivityServiceHandler(server, activitySvc)
	cairnv1connect.RegisterGearServiceHandler(server, connectadapter.NewGearServer(app.Gear))
	cairnv1connect.RegisterNotificationServiceHandler(server, connectadapter.NewNotificationServer(app.Notifications))
	cairnv1connect.RegisterAuthServiceHandler(server, connectadapter.NewAuthServer(app.Users, app.OIDCProviders))
	connecthttp.Mount(mux, server)
	logger.Info("connect-rpc services mounted", "services", []string{
		cairnv1connect.ActivityServiceName, cairnv1connect.NotificationServiceName, cairnv1connect.AuthServiceName,
		cairnv1connect.GearServiceName,
	})

	// AdminService chains the session resolver with a role-check
	// interceptor on its own server. Non-admins get CodePermissionDenied
	// before the handler runs.
	adminServer := connectrpc.NewServer(
		sessionInterceptor,
		connectadapter.ScopeInterceptor(),
		connectadapter.RequireAdminInterceptor(app.Users),
	)
	cairnv1connect.RegisterAdminServiceHandler(adminServer, connectadapter.NewAdminServer(app.Users, app.OIDCProviders))
	connecthttp.Mount(mux, adminServer)
	logger.Info("connect-rpc services mounted", "services", []string{cairnv1connect.AdminServiceName})
}
