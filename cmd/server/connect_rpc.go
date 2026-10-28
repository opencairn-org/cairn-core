package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/reflect/protoreflect"

	cairnv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	connectadapter "github.com/opencairn-org/cairn-core/internal/adapter/primary/connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
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
	// Binary content and visibility-gated reads share the REST machinery.
	activitySvc.Content = app.Content
	activitySvc.Attachments = app.Attachments
	activitySvc.Visibility = app.Visibility2
	activitySvc.Exports = app.Exports
	activitySvc.Searches = app.Searches
	activitySvc.Details = app.Details
	activitySvc.MapETag = mapImageETag
	cairnv1connect.RegisterActivityServiceHandler(server, activitySvc)
	cairnv1connect.RegisterGearServiceHandler(server, connectadapter.NewGearServer(app.Gear))
	cairnv1connect.RegisterNotificationServiceHandler(server, connectadapter.NewNotificationServer(app.Notifications))
	cairnv1connect.RegisterAuthServiceHandler(server, newAuthServer(app, logger))
	registerReadServices(server, app)
	cairnv1connect.RegisterSocialServiceHandler(server, connectadapter.NewSocialServer(app.Feeds, app.Content, app.Gear, mapImageETag))
	connecthttp.Mount(streamSafeMux{mux}, server)
	logger.Info("connect-rpc services mounted", "services", []string{
		cairnv1connect.ActivityServiceName, cairnv1connect.NotificationServiceName, cairnv1connect.AuthServiceName,
		cairnv1connect.GearServiceName, cairnv1connect.StatsServiceName, cairnv1connect.SocialServiceName,
		cairnv1connect.SegmentServiceName, cairnv1connect.MetricsServiceName, cairnv1connect.SettingsServiceName,
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
	connecthttp.Mount(streamSafeMux{mux}, adminServer)
	logger.Info("connect-rpc services mounted", "services", []string{cairnv1connect.AdminServiceName})
}

// streamingProcedures lists every cairn.v1 RPC with a streaming side, keyed
// by Connect path ("/pkg.Service/Method"), read from the proto descriptors so
// a new stream RPC is covered without a hand-kept list.
var streamingProcedures = func() map[string]bool {
	out := map[string]bool{}
	for _, fd := range []protoreflect.FileDescriptor{
		cairnv1.File_cairn_v1_activity_proto, cairnv1.File_cairn_v1_admin_proto, cairnv1.File_cairn_v1_auth_proto,
		cairnv1.File_cairn_v1_gear_proto, cairnv1.File_cairn_v1_metrics_proto, cairnv1.File_cairn_v1_notification_proto,
		cairnv1.File_cairn_v1_segment_proto, cairnv1.File_cairn_v1_stats_proto, cairnv1.File_cairn_v1_social_proto,
	} {
		svcs := fd.Services()
		for i := range svcs.Len() {
			svc := svcs.Get(i)
			ms := svc.Methods()
			for j := range ms.Len() {
				m := ms.Get(j)
				if m.IsStreamingServer() || m.IsStreamingClient() {
					out["/"+string(svc.FullName())+"/"+string(m.Name())] = true
				}
			}
		}
	}
	return out
}()

// streamSafeMux registers Connect handlers on the real mux with the
// server-wide WriteTimeout lifted for streaming RPCs: a server stream outlives
// any sane write deadline (the client holds it open for live updates), while
// unary calls keep the timeout as a backstop. Per-request deadlines still come
// from Connect-Timeout-Ms.
type streamSafeMux struct{ mux *http.ServeMux }

func (m streamSafeMux) Handle(pattern string, h http.Handler) {
	m.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if streamingProcedures[r.URL.Path] {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		}
		h.ServeHTTP(w, r)
	}))
}

// instanceInfo assembles GetInstanceInfo from the build metadata and config.
func instanceInfo(app *App) connectadapter.InstanceInfo {
	version, commit, built := buildInfo()
	info := connectadapter.InstanceInfo{
		Version:           version,
		Commit:            commit,
		FederationEnabled: app.FederationEnabled,
		OAuthEnabled:      app.OAuthEnabled,
		RegistrationOpen:  false, // invite-only; GetLoginMethods says requires_invite
	}
	if t, err := time.Parse(time.RFC3339, built); err == nil {
		info.BuildTime = t
	}
	return info
}

// newAuthServer wires the AuthService with its instance, session, quota and
// registration dependencies.
func newAuthServer(app *App, logger *slog.Logger) *connectadapter.AuthServer {
	authSvc := connectadapter.NewAuthServer(app.Users, app.OIDCProviders)
	authSvc.Instance = instanceInfo(app)
	authSvc.Sessions = app.Sessions
	authSvc.Activities = app.Activities
	if app.Quotas != nil {
		authSvc.Quotas = app.Quotas
	}
	authSvc.QuotaMaxActivities = app.QuotaMaxActivities
	if app.RedeemInvite != nil {
		authSvc.Invites = app.RedeemInvite
	}
	if app.Email != nil {
		authSvc.VerifyEmail = func(ctx context.Context, u domain.User) error {
			return sendVerificationEmail(ctx, app, logger, strings.TrimRight(app.PublicBaseURL, "/"), u)
		}
	}
	return authSvc
}

// registerReadServices mounts the aggregate and reference services (stats,
// segments, metrics, settings); nil repos stay nil interfaces.
func registerReadServices(server *connectrpc.Server, app *App) {
	statsSvc := connectadapter.NewStatsServer(app.Overviews, app.Content, app.Gear, mapImageETag)
	statsSvc.Stats = app.Stats
	cairnv1connect.RegisterStatsServiceHandler(server, statsSvc)
	cairnv1connect.RegisterSegmentServiceHandler(server, connectadapter.NewSegmentServer(app.SegmentViews))
	var metricsRepo port.MetricRepo
	if app.Metrics != nil {
		metricsRepo = app.Metrics
	}
	var athlete port.AthleteProfileRepo
	if app.AthleteProfiles != nil {
		athlete = app.AthleteProfiles
	}
	cairnv1connect.RegisterMetricsServiceHandler(server, connectadapter.NewMetricsServer(metricsRepo, app.HealthViews, athlete))
	var settings port.UserSettingsRepo
	if app.UserSettings != nil {
		settings = app.UserSettings
	}
	cairnv1connect.RegisterSettingsServiceHandler(server, connectadapter.NewSettingsServer(settings))
}
